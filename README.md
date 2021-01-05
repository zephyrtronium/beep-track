# track

[![Go Reference](https://pkg.go.dev/badge/github.com/zephyrtronium/beep-track.svg)](https://pkg.go.dev/github.com/zephyrtronium/beep-track)

Package track provides a (github.com/faiface/beep)[https://pkg.go.dev/github.com/faiface/beep@v1.0.2] Streamer with continuous playback and real-time stream insertion.

A Track defaults to an active streamer, filling in gaps with a silence streamer. Goroutines can set new active streamers concurrently with calls to Stream.
