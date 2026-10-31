#ifndef CAPTURED_AV_H
#define CAPTURED_AV_H

#include <libavcodec/avcodec.h>
#include <libavutil/hwcontext.h>
#include <libavutil/opt.h>
#include <libswscale/swscale.h>

typedef struct cap_enc {
	const AVCodec *codec;
	AVCodecContext *ctx;
	AVBufferRef *hw_frames; // frames the caller fills (hw path) or the upload target (cpu path)
	AVBufferPool *cpu_pool; // tight bgr0 buffers for the cpu path
	struct SwsContext *sws;
	AVFrame *conv;  // sws output (cpu path, when the encoder can't take bgr0)
	AVFrame *up;    // hw upload target (cpu path into a hw-only encoder)
	AVPacket *pkt;
	int src_w, src_h;
	enum AVPixelFormat src_fmt; // what the caller fills
	int force_key;
} cap_enc;

int cap_supports(const AVCodec *c, enum AVPixelFormat f);
enum AVPixelFormat cap_first_sw(const AVCodec *c);
const char *cap_err(int e);
int cap_enc_open(cap_enc *e, const char *name, int src_w, int src_h, int w, int h, int fps,
	int64_t bitrate, int gop, AVBufferRef *hw_dev, enum AVPixelFormat hw_fmt,
	enum AVPixelFormat sw_fmt, AVDictionary **opts);
AVFrame *cap_enc_frame(cap_enc *e);
int cap_enc_send(cap_enc *e, AVFrame *f);
int cap_enc_recv(cap_enc *e);
void cap_enc_close(cap_enc *e);

#endif
