//go:build darwin

package av

/*
#cgo darwin LDFLAGS: -framework CoreVideo -framework VideoToolbox
#include <CoreVideo/CoreVideo.h>
#include <libavcodec/avcodec.h>
#include <libavutil/hwcontext.h>
#include "videotoolbox_darwin.h"

static void cap_vt_free(void *opaque, uint8_t *data) {
	CVBufferRelease((CVBufferRef)data);
}

// Attach a retained CVPixelBufferRef to f, replacing the pooled surface.
// The buffer is released when the frame's last ref dies. pixbuf is
// consumed on failure.
static int cap_vt_wrap(AVFrame *f, void *pixbuf, int w, int h) {
	av_buffer_unref(&f->buf[0]);
	memset(f->data, 0, sizeof f->data);
	memset(f->linesize, 0, sizeof f->linesize);
	f->buf[0] = av_buffer_create((uint8_t *)pixbuf, 0, cap_vt_free, NULL, 0);
	if (!f->buf[0]) {
		CVBufferRelease((CVBufferRef)pixbuf);
		return AVERROR(ENOMEM);
	}
	f->data[3] = (uint8_t *)pixbuf;
	f->width = w;
	f->height = h;
	f->format = AV_PIX_FMT_VIDEOTOOLBOX;
	return 0;
}

// GPU blit pixbuf into f's pooled surface via VTPixelTransferSession.
// Consumes pixbuf on success and failure alike.
static int cap_vt_blit(AVFrame *dst, void *pixbuf) {
	int st = cap_vt_transfer(dst->data[3], pixbuf);
	CVBufferRelease((CVBufferRef)pixbuf);
	return st;
}
*/
import "C"

import "unsafe"

// FillVideoToolbox points f at a ScreenCaptureKit CVPixelBuffer of pw x ph.
// It wraps pixbuf directly when it matches the frame's size (zero copy),
// and GPU-blits into the pooled surface when the encoder asked for a
// different size. pixbuf must already be retained and is always consumed,
// even on failure.
func (f Frame) FillVideoToolbox(pixbuf unsafe.Pointer, pw, ph int) error {
	var ret C.int
	if pw == int(f.f.width) && ph == int(f.f.height) {
		ret = C.cap_vt_wrap(f.f, pixbuf, C.int(pw), C.int(ph))
	} else {
		ret = C.cap_vt_blit(f.f, pixbuf)
	}
	if ret < 0 {
		return avErr("videotoolbox frame", ret)
	}
	return nil
}
