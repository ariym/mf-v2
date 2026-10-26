#!/usr/bin/env python3
"""Verify packaged v2 flows using a temporary catalog and fixture ML programs.

Build mf and the two v2 release archives first; see docs/hub.md. The test starts
and stops only its own daemon. It does not install or prepare production models.
"""
import argparse
import json
import socket
import sqlite3
import subprocess
import tarfile
import tempfile
from pathlib import Path


FIXTURE_PROGRAM = '''#!/usr/bin/env python3
import sys,json,pathlib,hashlib
key=pathlib.Path(sys.argv[0]).name
if sys.argv[1]=='describe':
 print(json.dumps({'program_name':key,'version':1}));sys.exit(0)
r=json.load(sys.stdin)
if sys.argv[1]=='preflight':print('{}');sys.exit(0)
if key=='gliner':
 docs=json.loads(pathlib.Path(r['documents_path']).read_text())['documents']
 provenance={'program':'gliner','version':1,'model':'fixture','model_revision':'fixture',
 'model_archive_sha256':'a'*64,'backend':'cpu','threshold':0.35,
 'documents_sha256':r['expected_documents_sha256'],'window_tokens':384,'overlap_tokens':64}
 print(json.dumps({'names':[],'documents':[{'tag_id':d['tag_id'],'names':[]} for d in docs],
 'provenance':provenance}))
elif key=='pyannote':print(json.dumps({'provenance':{},'result':{'diarization':[]}}))
else:
 path=r['media'][0]
 value={'transcript':{'text':'Alice','language':'en',
 'segments':[{'text':'Alice','start':0,'end':1}]},'raw':{},
 'media':{'path':path,'sha256':hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest(),
 'stream_index':0}}
 print(json.dumps({'provenance':{},'result':{'media_path':path,'speech':value}}))
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--projects-root', type=Path,
                        default=Path(__file__).resolve().parents[2])
    parser.add_argument('--build-dir', type=Path, required=True,
                        help='Directory with mf, transcribe/ and names/ release outputs')
    args = parser.parse_args()
    projects, build = args.projects_root.resolve(), args.build_dir.resolve()
    # Keep the Unix control socket below macOS's pathname limit.
    root = Path(tempfile.mkdtemp(prefix='mf-prerequisite-e2e-', dir='/tmp'))
    binary, config = build / 'mf', root / 'mf.yaml'

    def run(*arguments):
        result = subprocess.run([str(binary), '--config', str(config),
                                 *map(str, arguments)], text=True, capture_output=True,
                                timeout=90)
        if result.returncode:
            raise RuntimeError(f'{arguments}: {result.stderr}\n{result.stdout}')
        return result.stdout.strip()

    run('init', '-y')
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    config.write_text(config.read_text().replace('server_port: 11747',
                                                f'server_port: {port}'))
    video = root / 'original.mkv'
    subprocess.run(['ffmpeg', '-v', 'error', '-f', 'lavfi', '-i',
                    'color=size=16x16:duration=1', '-f', 'lavfi', '-i',
                    'sine=duration=1', '-c:v', 'ffv1', '-c:a', 'pcm_s16le', str(video)],
                   check=True)
    segment = int(run('segments', 'add', video, '00:00:00.100000', '00:00:00.800000'))
    for key in ['speech', 'speaker', 'diarized_transcript']:
        run('tagtypes', 'add', projects / 'mf-v2/internal/hub/testdata' / f'{key}.v2.json')
    run('tagtypes', 'add', projects /
        'mf-flow-recognize-transcript-names/schemas/transcript_names.v1.json')
    for key in ['pyannote', 'whisperx', 'gliner']:
        executable = root / key
        executable.write_text(FIXTURE_PROGRAM)
        executable.chmod(0o755)
        run('programs', 'link', key, executable, '--version', '1',
            '--input-formats', '[{"container":".wav"}]')

    # Register the local packages before starting the temporary daemon. Hub
    # transport/installation is covered by the client and hub Go test suites.
    with sqlite3.connect(root / 'mf.db') as database:
        for name, folder in [('transcribe', 'transcribe'),
                             ('recognize_transcript_names', 'names')]:
            manifest = json.loads((build / folder / 'manifest.json').read_text())
            artifact = manifest['artifacts'][0]
            archive = build / folder / artifact['url'].rsplit('/', 1)[1]
            with tarfile.open(archive) as package:
                package.extractall(root / folder, filter='data')
            executable = root / folder / 'bin' / name
            database.execute('''INSERT INTO flow(key,version,manifest,source,install_dir,
                executable,platform_os,platform_arch,artifact_sha256,created_at,modified_at)
                VALUES(?,?,?,?,?,?,?,?,?,?,?)''',
                (name, 2, json.dumps(manifest), 'isolated-e2e', '', str(executable),
                 artifact['os'], artifact['arch'], artifact['sha256'],
                 '2026-10-26T00:00:00Z', '2026-10-26T00:00:00Z'))

    try:
        result = json.loads(run('jobs', 'run', 'recognize_transcript_names@2',
                                '--segment', segment, '--wait', '--json'))
        assert result['state'] == 'completed', result
        with sqlite3.connect(root / 'mf.db') as database:
            jobs = database.execute('''SELECT flow_key,terminal_state FROM job_metadata
                ORDER BY created_at''').fetchall()
            links = database.execute('''SELECT d.key,s.start,s.end,s.media_file_id
                FROM tag t JOIN tagtype d ON d.id=t.tag_type_id
                JOIN segment_tags st ON st.tag_id=t.id JOIN segments s ON s.id=st.segment_id
                WHERE d.key='transcript_names' ''').fetchall()
            assert len(jobs) == 3 and all(state == 'completed' for _, state in jobs), jobs
            assert len(links) == 1 and links[0][1:3] == (
                '00:00:00.100000', '00:00:00.800000'), links
            assert database.execute('SELECT count(*) FROM audio_artifacts').fetchone()[0] == 1
        print(json.dumps({'root': str(root), 'state': result['state'],
                          'jobs': jobs, 'result_links': links}, indent=2))
    finally:
        subprocess.run([str(binary), '--config', str(config), 'daemon', 'stop', '-f'],
                       text=True, capture_output=True, timeout=30)


if __name__ == '__main__':
    main()
