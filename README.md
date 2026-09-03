# captured
Capture process for the agent

## But why?
Especially for an agent process written in one language, the display capture sidde of Distance should be separate.
captured is an unbelivably (but predictibly!) dumb capture agent for Windows, Linux, and MacOS.

## How does it work?
captured listens over two sockets; captured-media and captured-control.
As on the tin, Media sends the actual video (or audio) content being recorded, while control is a control plane for it.

## Linux capture sources

Pick one with `--source`:

| Source | Notes |
|--------|-------|
| `kms` (default) | libdrm/libgbm readback via purego. Needs read access to `/dev/dri/card*` and a CRTC bound to a connected display; falls back to a synthetic BGRA pattern when readback is unavailable. |
| `pipewire` | Screen capture through the compositor. Works on Wayland with no CRTC bound and needs no DRM permissions. |
| `x11` | Lists the default X screen; pixel readback is not implemented yet. |

### pipewire

```sh
captured --source pipewire
```

Requires `gst-launch-1.0` (`gstreamer1.0-tools`) and `gstreamer1.0-pipewire`.

Must run **as the desktop session user**, from inside that session, because it
talks to `org.gnome.Mutter.ScreenCast` on the session bus. GNOME/Mutter only for
now: the freedesktop portal (`org.freedesktop.portal.Desktop.ScreenCast`) would
be compositor-agnostic but requires interactive consent through a dialog, which a
daemon cannot satisfy.
