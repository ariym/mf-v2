# Test face recognition accuracy locally

The current local build can run train, enroll, and identify through `mf programs exec`. The face flows have not been published to the hub, so these instructions use the linked program. Program execution writes artifacts directly; it does not create mf jobs or ingest timeline tags. No daemon is needed.

1. Create a separate experiment directory and link the built program.

   ```sh
   export MF_BIN=/Users/ari/Developer/Projects/mf-v2/dist/mf
   export FACE_REPO=/Users/ari/Developer/Projects/mf-program-insightface
   export FACE_PYTHON="$FACE_REPO/dist/runtime/libexec/python/bin/python3"
   export MF_INSIGHTFACE_MODEL_CACHE="$FACE_REPO/dist/model-cache"

   mkdir -p "$HOME/face-accuracy/run-001"
   cd "$HOME/face-accuracy/run-001"
   "$MF_BIN" init -y
   "$MF_BIN" programs link insightface "$FACE_REPO/dist/runtime/bin/insightface" --version 1
   "$MF_BIN" programs exec insightface -- ready
   ```

   Stay in this directory for the commands below. This avoids a current passthrough parsing issue with a global `--config` flag on `programs exec`. The model cache above was already prepared during local validation. If `ready` reports missing models, run `"$MF_BIN" programs exec insightface -- prepare` once, then repeat `ready`. Only `prepare` needs network access.

2. Supply your own labeled videos and reference photos using this layout.

   ```text
   data/
     train/
       alice/session-1/*.mp4
       alice/session-2/*.mp4
       bob/session-1/*.mp4
       bob/session-2/*.mp4
     references/alice.jpg
     references/bob.jpg
     enroll/alice/*.mp4
     enroll/bob/*.mp4
     test/alice/*.mp4
     test/bob/*.mp4
     test/unknown/*.mp4
   ```

   Training and benchmark clips must each show one person throughout. Reference photos must contain exactly one usable face. Enrollment can use group videos, provided the reference identifies the intended person. `unknown` clips must show people who are not enrolled as Alice or Bob.

   Keep calibration, enrollment, and test recordings in separate sessions. Re-encoding or trimming a recording does not make it independent. Two identities with two training sessions each is the software minimum; use more identities, sessions, poses, lighting conditions, and unknown people for a useful evaluation. Keep test recordings untouched while adjusting training data or settings.

3. Generate requests with absolute paths.

   This script expects the layout above. Add identities to `people` and provide their folders/photos to extend the experiment. Use a new experiment directory for a rerun; existing result directories are deliberately rejected.

   ```sh
   "$FACE_PYTHON" - <<'PY'
   import json
   from pathlib import Path

   root = Path.cwd().resolve()
   people = {"alice": "Alice", "bob": "Bob"}
   options = {"backend": "cpu", "analysis_fps": 5}
   requests, responses, results = [root / name for name in ("requests", "responses", "results")]
   for directory in (requests, responses, results):
       directory.mkdir(exist_ok=True)

   def clips(directory):
       found = sorted(directory.glob("*.mp4"))
       if not found:
           raise SystemExit(f"No .mp4 clips in {directory}")
       return [str(path.resolve()) for path in found]

   def save(name, request):
       output = results / name
       output.mkdir()  # Each program run requires an existing EMPTY directory.
       request["output_dir"] = str(output)
       (requests / f"{name}.json").write_text(json.dumps(request, indent=2) + "\n")

   labels = {}
   for person in people:
       sessions = sorted(path for path in (root / "data/train" / person).iterdir() if path.is_dir())
       if len(sessions) < 2:
           raise SystemExit(f"Need at least two training sessions for {person}")
       for session in sessions:
           for path in clips(session):
               labels[path] = {"person_id": person, "session_id": session.name}
   save("train", {
       "operation": "train_matcher", "media": list(labels), "training_labels": labels,
       "options": dict(options, target_fmr=0.01),
   })

   matcher = str(results / "train/matcher.json")
   profiles = []
   for person, label in people.items():
       reference = root / "data/references" / f"{person}.jpg"
       if not reference.is_file():
           raise SystemExit(f"Missing reference: {reference}")
       save(f"enroll-{person}", {
           "operation": "enroll", "person_id": person, "label": label,
           "reference": str(reference.resolve()), "matcher": matcher,
           "media": clips(root / "data/enroll" / person), "options": options,
       })
       profiles.append(str(results / f"enroll-{person}/face_profile.json"))

   truth = {}
   for folder, person in list((person, person) for person in people) + [("unknown", None)]:
       for path in clips(root / "data/test" / folder):
           truth[path] = person
   save("test", {
       "operation": "identify", "media": list(truth), "matcher": matcher,
       "profiles": profiles, "options": options,
   })
   (requests / "truth.json").write_text(json.dumps(truth, indent=2) + "\n")
   print(f"Requests: {requests}")
   PY
   ```

   The same sampling and filtering settings are used in every request because they are part of the embedding-space fingerprint. Changing them requires a compatible new matcher and profiles. Switching between CPU and Core ML is supported.

4. Train the matcher, then enroll each person.

   ```sh
   "$MF_BIN" programs exec insightface -- run \
     < requests/train.json > responses/train.json

   "$MF_BIN" programs exec insightface -- run \
     < requests/enroll-alice.json > responses/enroll-alice.json

   "$MF_BIN" programs exec insightface -- run \
     < requests/enroll-bob.json > responses/enroll-bob.json
   ```

   Continue only after each command succeeds. Inspect `results/train/evaluation.json` for calibration trial counts, empirical false-match and false-rejection rates, and the reported confidence interval. A 1% training target is not a guarantee of 1% on new recordings. Insufficient examples, ambiguous training clips, and uncertain enrollment selections fail rather than producing a usable result.

   Keep stdout responses outside the operation's output directory: shell redirection creates its file before the program checks that the output directory is empty.

5. Run held-out inference through mf.

   ```sh
   "$MF_BIN" programs exec insightface -- preflight < requests/test.json
   "$MF_BIN" programs exec insightface -- run \
     < requests/test.json > responses/test.json
   ```

   Predictions are under `result.items[].face_tracks.tracks` in `responses/test.json`. Each track contains `person_id`, `label`, scores, candidates, and timed appearances. `person_id: null` means unknown. Cosine scores are similarities, not probabilities. `results/test/analysis.json` retains observations and artifact references.

6. Calculate accuracy, processing time, and memory on both CPU and Core ML.

   ```sh
   "$FACE_PYTHON" "$FACE_REPO/scripts/benchmark.py" \
     --program "$("$MF_BIN" programs path insightface)" \
     --request "$PWD/requests/test.json" \
     --truth "$PWD/requests/truth.json" \
     --output "$PWD/results/benchmark" \
     --independent-sessions

   "$FACE_PYTHON" -m json.tool results/benchmark/benchmark.json
   ```

   The helper resolves the linked executable through mf, then runs that executable directly for each backend. It overrides the request's output directory, so the earlier inference does not need to be deleted. `results/benchmark` must not already exist. Only use `--independent-sessions` when you have actually separated recording sessions; the helper also checks for identical source hashes in calibration/enrollment.

   Compare these fields per backend:

   | Field | Meaning |
   | --- | --- |
   | `frame_accuracy` | Fraction of sampled frames with the expected identity/unknown result |
   | `false_match_rate` | Unknown-person sampled frames assigned an enrolled identity |
   | `false_rejection_rate` | Known-person sampled frames missing the expected identity, including wrong identities and missed detections |
   | `frames`, `detected_frames` | Sample count and face detection coverage |
   | `wall_seconds`, `peak_rss_bytes` | Whole inference invocation time and peak process memory |

   These are sampled-frame metrics, not independent trial counts. Unknown frames with no usable detection count as correct unknowns, so inspect detection coverage alongside accuracy. Use held-out results to assess the calibration target; do not call a small sample with zero false matches proof of a population error rate. Report wrong known-person identities separately when reviewing failures, since this utility includes them in false rejection.

   The quantitative helper accepts single-person clips. To evaluate crossings, occlusions, shot changes, and group scenes, run an additional `identify` request and compare its track intervals, identities, and boxes against manual time-based labels. Those scenes are not scored by this helper.
