# captured
Capture process for the agent

## But why?
Especially for an agent process written in one language, the display capture sidde of Distance should be separate.
captured is an unbelivably (but predictibly!) dumb capture agent for Windows, Linux, and MacOS.

## How does it work?
captured listens over two sockets; captured-media and captured-control.
As on the tin, Media sends the actual video (or audio) content being recorded, while control is a control plane for it.
