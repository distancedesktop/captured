#ifndef CAPTURED_CAPSCK_DARWIN_H
#define CAPTURED_CAPSCK_DARWIN_H

#include <stdint.h>

// Blocking ScreenCaptureKit stream that hands out retained CVPixelBufferRefs.
// Buffers are 32BGRA. The caller releases them with cap_sck_pb_release.

// cap_sck_start opens a stream for cg displayID at w x h pixels, fps.
// Returns an opaque handle or NULL with errbuf set.
void *cap_sck_start(uint32_t displayID, int w, int h, int fps, char *errbuf, int errlen);

// cap_sck_next returns the newest frame as a retained CVPixelBufferRef.
// 0 = frame in *out, 1 = timed out, <0 = stream error (errbuf set).
int cap_sck_next(void *h, int timeout_ms, void **out, char *errbuf, int errlen);

// cap_sck_pb_copy locks pb and copies its BGRA pixels into dst (w*h*4 bytes).
// Returns 0 or <0 on unexpected layout.
int cap_sck_pb_copy(void *pb, void *dst, int w, int h);

void cap_sck_pb_release(void *pb);
void cap_sck_pb_size(void *pb, int *w, int *h);

// cap_sck_stop tears the stream down. Safe from any thread while another
// is blocked in cap_sck_next: it wakes waiters, which then return the
// stored error.
void cap_sck_stop(void *h);

#endif
