# captured

A dumb capture daemon. It grabs a display and hands raw BGRA frames to whoever asks (the Distance agent). Encoding is not its job.

## Layout

`main.go` is the daemon. Everything that actually captures lives in `pipelines/`, one file per pipeline, named `<source>_<capturer>_<os>.go`:

| Pipeline | File | What it does |
|---|---|---|
| `kms_nvidia` | `kms_nvidia_linux.go` | Reads the NVIDIA scanout buffer through EGL: imports the tiled dma-buf with its modifier and reads it back with `glReadPixels`. Needs `CAP_SYS_ADMIN`. No cursor (it's on its own plane). |
| `pipewire_mutter` | `pipewire_mutter_linux.go` | GNOME Mutter ScreenCast → PipeWire → `gst-launch-1.0`. Cursor included. Frames only arrive when the screen changes. Run as the desktop user. |
| `kms_linear` | `kms_linear_linux.go` | Maps a linear scanout buffer and copies it (virtio-gpu, simpledrm, some iGPUs). Refuses tiled buffers. |
| `screencapturekit` | `screencapturekit_darwin.go` | macOS ScreenCaptureKit via sckit-go. |

Shared bits: `pipeline.go` (interface + registry), `stream.go` (pacing, buffer reuse, dropping stale frames), `drm_linux.go`, `egl_linux.go`. Every native library is loaded at runtime with purego, so there's no cgo.

Nothing falls back to a fake image. If a pipeline can't capture, it says why.

## Running

```sh
go build -o captured .
./captured --list                      # every pipeline, whether it works here, and its displays
sudo setcap cap_sys_admin+ep captured  # for kms_nvidia, same as Sunshine's KMS capture
./captured                             # --pipeline auto picks the best one that works
./captured --pipeline pipewire_mutter
```

Flags: `--pipeline`, `--socket` (`/tmp/captured.socket`), `--media-socket` (`/tmp/captured-media.socket`), `--listen :9090` for TCP control, `--list`.

## Protocol

Control is JSON lines on the control socket.

| Request | Reply |
|---|---|
| `{"type":"list-displays"}` | `{"type":"displays","displays":[{"id","name","width","height","x","y","refresh_rate"}]}` |
| `{"type":"start-stream","display_id":0,"fps":60}` | `{"type":"stream-started","socket","format":"bgra","pipeline","width","height"}` |
| `{"type":"stop-stream"}` | `{"type":"stream-stopped"}` |
| `{"type":"stats"}` | `{"type":"stats","streaming","stats":{"frames","dropped","grab_avg_ms","grab_max_ms"}}` |
| `{"type":"info"}` | `{"type":"info","version","formats","pipeline","pipelines"}` |

Errors come back as `{"type":"error","error":"..."}`.

Media: after `stream-started`, connect to the media socket and read `[u32 BE width][u32 BE height][width*height*4 BGRA]` per frame. There's one frame of buffering; if you read too slowly, stale frames get dropped instead of queueing up. Disconnecting stops the stream.

## Bench

`go run ./cmd/capbench -pid $(pgrep -x captured) -secs 5` drives a running captured and prints fps, frame gaps, duplicate frames and CPU.
