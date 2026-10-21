package config

import "fmt"

type RenderConfig struct {
	FFmpeg       string `yaml:"ffmpeg" json:"ffmpeg"`
	VideoCodec   string `yaml:"video_codec" json:"video_codec"`
	Preset       string `yaml:"preset" json:"preset"`
	CRF          int    `yaml:"crf" json:"crf"`
	Width        int    `yaml:"width" json:"width"`
	Height       int    `yaml:"height" json:"height"`
	FPS          int    `yaml:"fps" json:"fps"`
	PixelFormat  string `yaml:"pixel_format" json:"pixel_format"`
	AudioCodec   string `yaml:"audio_codec" json:"audio_codec"`
	AudioBitrate string `yaml:"audio_bitrate" json:"audio_bitrate"`
	SampleRate   int    `yaml:"sample_rate" json:"sample_rate"`
	Channels     int    `yaml:"channels" json:"channels"`
	MP3Codec     string `yaml:"mp3_codec" json:"mp3_codec"`
	MP3Quality   int    `yaml:"mp3_quality" json:"mp3_quality"`
}

func DefaultRender() RenderConfig {
	return RenderConfig{FFmpeg: "ffmpeg", VideoCodec: "libx264", Preset: "medium", CRF: 20, Width: 1920, Height: 1080, FPS: 30, PixelFormat: "yuv420p", AudioCodec: "aac", AudioBitrate: "192k", SampleRate: 48000, Channels: 2, MP3Codec: "libmp3lame", MP3Quality: 2}
}
func (r RenderConfig) Validate() error {
	if r.FFmpeg == "" || r.VideoCodec == "" || r.AudioCodec == "" || r.MP3Codec == "" || r.PixelFormat == "" || r.Preset == "" || r.AudioBitrate == "" {
		return fmt.Errorf("sql_to_ffmpeg executable, codecs, preset, pixel format and bitrate must not be empty")
	}
	if r.Width < 2 || r.Width%2 != 0 || r.Height < 2 || r.Height%2 != 0 || r.FPS < 1 || r.CRF < 0 || r.CRF > 51 || r.SampleRate < 1 || r.Channels < 1 || r.Channels > 2 || r.MP3Quality < 0 || r.MP3Quality > 9 {
		return fmt.Errorf("invalid sql_to_ffmpeg dimensions, frame rate, quality, sample rate, or channels")
	}
	return nil
}
