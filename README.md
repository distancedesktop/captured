# captured

Captures a display and encodes it in the same process. The Distance agent spawns it as a child, drives it over JSON-RPC on stdin/stdout and reads encoded video from fd 3. Frames stay on the GPU from capture to encoder wherever the platform allows.

## Layout

`main.go`, `server.go` (JSON-RPC and properties) and `writer.go` (media records) are the process. Encoding is in `av/`, a small cgo wrapper around libavcodec. Capture lives in `pipelines/`, one file per pipeline, named `<source>_<capturer>_<os>.go`:

| Pipeline | File | What it does |
|---|---|---|
| `kms_nvidia` | `kms_nvidia_linux.go` | Imports the NVIDIA scanout dma-buf into EGL, blits (and scales) it into a texture, copies that into a CUDA frame over CUDA-GL interop and hands it to NVENC. Nothing touches system memory. Falls back to `glReadPixels` for software encoders. Needs `CAP_SYS_ADMIN`. No cursor (it's on its own plane). |
| `pipewire_mutter` | `pipewire_mutter_linux.go` | GNOME Mutter ScreenCast → PipeWire → `gst-launch-1.0`. Cursor included. Frames only arrive when the screen changes. Run as the desktop user. |
| `kms_linear` | `kms_linear_linux.go` | Maps a linear scanout buffer and copies it (virtio-gpu, simpledrm, some iGPUs). Refuses tiled buffers. |
| `screencapturekit` | `screencapturekit_darwin.go` | macOS ScreenCaptureKit via sckit-go. |
| `dxgi` | `dxgi_windows.go` | Windows DXGI Desktop Duplication, staging texture copy. No cursor. Repeats the last frame when the screen is idle and recreates the duplication on access loss (UAC, mode change, fullscreen). |

Shared bits: `pipeline.go` (interface and registry), `session.go` (the capture and encode loop, encoder choice, stats), `drm_linux.go`, `egl_linux.go`, `cuda_linux.go`. Capture libraries are loaded at runtime with purego (golang.org/x/sys/windows on Windows). libav is linked with cgo.

Nothing falls back to a fake image. If a pipeline can't capture, it says why.

## Building

Needs a C compiler and the libavcodec, libavutil and libswscale dev packages (found with pkg-config).

```sh
sudo apt install build-essential pkg-config libavcodec-dev libavutil-dev libswscale-dev  # Debian/Ubuntu
brew install ffmpeg pkg-config                                                         # macOS
pacman -S mingw-w64-ucrt-x86_64-{gcc,pkgconf,ffmpeg}                                   # Windows, MSYS2 UCRT64
go build -o captured .
sudo setcap cap_sys_admin+ep captured  # for kms_nvidia, same as Sunshine's KMS capture
./captured --list                      # every pipeline, whether it works here, and its displays
```

## Running

captured is meant to be spawned. stdin/stdout carry JSON-RPC 2.0, one message per line. Logs go to stderr. Encoded records go to `--media` (default `fd:3`, an inherited fd or Windows handle, or a file path). When stdin closes, captured stops and exits.

## Control

| Method | Params | Result |
|---|---|---|
| `list` | none | pipelines, whether each works here and its displays |
| `get` | `["key", ...]` or nothing for all | `{key: value}` |
| `set` | `{key: value}` | the changed keys and their new values |
| `start` | `{key: value}` (optional, applied like `set`) | every property |
| `stop` | none | `{}` |

captured sends a `stopped` notification with `{"error": "..."}` when a stream ends without `stop`.

Settable properties:

| Key | Default | Notes |
|---|---|---|
| `pipeline` | `auto` | a pipeline name, or `auto` for the best one that works |
| `display` | `0` | display id from `list` |
| `video.codec` | `h264` | `h264`, `hevc`, `av1` |
| `video.encoder` | `auto` | a libav encoder name (`h264_nvenc`, `hevc_vaapi`, `libx264`, ...). `auto` tries hardware first |
| `video.hw_device` | `""` | hardware device for encoders that take one. For VAAPI this is a DRM render node (`/dev/dri/renderD128`); empty tries each `/dev/dri/renderD*` and then libav's default. For NVENC this is the CUDA GPU index |
| `video.fps` | `60` | |
| `video.width`, `video.height` | `0` | output size, 0 keeps the capture size |
| `video.bitrate` | `10000000` | bits per second. Live on NVENC and x264, the encoder reopens on the others |
| `video.gop` | `0` | frames between keyframes, 0 means only on request |
| `video.preset` | `""` | encoder preset, empty picks a low latency one |
| `video.options` | `{}` | extra libav encoder options |
| `video.keyframe` | | set to `true` to force a keyframe on the next frame |

Changing anything except bitrate and keyframe while streaming restarts the stream. `audio.*` keys are reserved and return an error for now.

Read-only: `version`, `state`, `video.active_pipeline`, `video.active_encoder`, `video.hw` (the hardware frame type, empty for system memory), `video.size`, and `stats.fps`, `stats.frames`, `stats.bitrate`, `stats.capture_avg_ms`, `stats.encode_avg_ms`, `stats.encode_max_ms`, `stats.dropped`, `stats.error`.

```json
{"jsonrpc":"2.0","id":1,"method":"start","params":{"video.encoder":"h264_nvenc","video.width":1920,"video.height":1080}}
{"jsonrpc":"2.0","id":2,"method":"set","params":{"video.bitrate":4000000}}
{"jsonrpc":"2.0","id":3,"method":"get","params":["stats.fps","stats.encode_avg_ms"]}
```

## Media

Every record is a 16 byte header and a payload:

```
u32 BE payload length | u8 type | u8 flags | u16 reserved | u64 BE pts (µs) | payload
```

- type `2` is a format record, a JSON object with `codec`, `encoder`, `pipeline`, `hw`, `width`, `height`, `fps` and `display`. One comes before the first packet of every stream, including restarts.
- type `0` is one encoded access unit (Annex B for H.264 and HEVC, OBU for AV1). Flag bit 0 marks a keyframe.

If the reader falls behind and the queue fills, packets are dropped until the next keyframe, and captured asks the encoder for one.

## Bench

`go run ./cmd/capbench -captured ./captured -duration 10s -set video.encoder=h264_nvenc` spawns captured, streams, and prints fps, packet gaps, bitrate, keyframes, captured's own stats and its CPU use. `-o out.h264` keeps the bitstream and `-bitrate-at 5s=2000000` changes the bitrate mid run.
