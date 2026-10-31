#include "av.h"
#include <libavutil/imgutils.h>
#include <string.h>

static const enum AVPixelFormat *pix_fmts(const AVCodec *c) {
#if LIBAVCODEC_VERSION_INT >= AV_VERSION_INT(61, 13, 100)
	const void *out = NULL;
	if (avcodec_get_supported_config(NULL, c, AV_CODEC_CONFIG_PIX_FORMAT, 0, &out, NULL) < 0)
		return NULL;
	return out;
#else
	return c->pix_fmts;
#endif
}

int cap_supports(const AVCodec *c, enum AVPixelFormat f) {
	const enum AVPixelFormat *p = pix_fmts(c);
	for (; p && *p != AV_PIX_FMT_NONE; p++)
		if (*p == f)
			return 1;
	return 0;
}

enum AVPixelFormat cap_first_sw(const AVCodec *c) {
	static const enum AVPixelFormat pref[] = {AV_PIX_FMT_NV12, AV_PIX_FMT_YUV420P};
	for (int i = 0; i < 2; i++)
		if (cap_supports(c, pref[i]))
			return pref[i];
	const enum AVPixelFormat *p = pix_fmts(c);
	for (; p && *p != AV_PIX_FMT_NONE; p++) {
		const AVPixFmtDescriptor *d = av_pix_fmt_desc_get(*p);
		if (d && !(d->flags & AV_PIX_FMT_FLAG_HWACCEL))
			return *p;
	}
	return AV_PIX_FMT_NONE;
}

const char *cap_err(int e) {
	static __thread char buf[AV_ERROR_MAX_STRING_SIZE];
	av_strerror(e, buf, sizeof buf);
	return buf;
}

static AVBufferRef *make_frames(AVBufferRef *dev, enum AVPixelFormat hw, enum AVPixelFormat sw, int w, int h) {
	AVBufferRef *ref = av_hwframe_ctx_alloc(dev);
	if (!ref)
		return NULL;
	AVHWFramesContext *fc = (AVHWFramesContext *)ref->data;
	fc->format = hw;
	fc->sw_format = sw;
	fc->width = w;
	fc->height = h;
	fc->initial_pool_size = hw == AV_PIX_FMT_VAAPI ? 8 : 0;
	if (av_hwframe_ctx_init(ref) < 0) {
		av_buffer_unref(&ref);
		return NULL;
	}
	return ref;
}

// Opens an encoder. hw_fmt != NONE with hw_dev: the caller fills hw frames of
// sw_fmt at w x h (zero copy). Otherwise the caller fills tight bgr0 frames of
// src_w x src_h, converted/uploaded here as the encoder needs.
int cap_enc_open(cap_enc *e, const char *name, int src_w, int src_h, int w, int h, int fps,
	int64_t bitrate, int gop, AVBufferRef *hw_dev, enum AVPixelFormat hw_fmt,
	enum AVPixelFormat sw_fmt, AVDictionary **opts) {
	int ret;
	memset(e, 0, sizeof *e);
	e->src_w = src_w;
	e->src_h = src_h;
	if (!(e->codec = avcodec_find_encoder_by_name(name)))
		return AVERROR_ENCODER_NOT_FOUND;
	if (!(e->ctx = avcodec_alloc_context3(e->codec)) || !(e->pkt = av_packet_alloc()))
		return AVERROR(ENOMEM);
	AVCodecContext *c = e->ctx;
	c->width = w;
	c->height = h;
	c->time_base = (AVRational){1, 1000000};
	c->framerate = (AVRational){fps, 1};
	c->bit_rate = bitrate;
	if (strcmp(name, "libsvtav1") != 0) { // svt only takes maxrate with crf
		c->rc_max_rate = bitrate;
		c->rc_buffer_size = (int)(bitrate / fps);
	}
	c->gop_size = gop;
	c->max_b_frames = 0;
	c->flags |= AV_CODEC_FLAG_LOW_DELAY;
	c->color_range = AVCOL_RANGE_MPEG;
	c->colorspace = AVCOL_SPC_BT709;
	c->color_primaries = AVCOL_PRI_BT709;
	c->color_trc = AVCOL_TRC_BT709;

	if (hw_fmt != AV_PIX_FMT_NONE && hw_dev) {
		if (!cap_supports(e->codec, hw_fmt))
			return AVERROR(ENOTSUP);
		if (!(e->hw_frames = make_frames(hw_dev, hw_fmt, sw_fmt, w, h)))
			return AVERROR(EINVAL);
		c->pix_fmt = hw_fmt;
		c->hw_frames_ctx = av_buffer_ref(e->hw_frames);
		e->src_fmt = hw_fmt;
	} else {
		e->src_fmt = AV_PIX_FMT_BGR0;
		int direct = w == src_w && h == src_h && (cap_supports(e->codec, AV_PIX_FMT_BGR0) || cap_supports(e->codec, AV_PIX_FMT_BGRA));
		enum AVPixelFormat in = cap_supports(e->codec, AV_PIX_FMT_BGR0) ? AV_PIX_FMT_BGR0 : AV_PIX_FMT_BGRA;
		e->cpu_fmt = direct ? in : AV_PIX_FMT_BGR0;
		if (!direct) {
			in = cap_first_sw(e->codec);
			int hwonly = in == AV_PIX_FMT_NONE;
			if (hwonly) {
				// e.g. h264_vaapi: upload nv12 into hw surfaces.
				enum AVPixelFormat hf = AV_PIX_FMT_NONE;
				if (cap_supports(e->codec, AV_PIX_FMT_VAAPI)) hf = AV_PIX_FMT_VAAPI;
				if (hf == AV_PIX_FMT_NONE || !hw_dev)
					return AVERROR(ENOTSUP);
				in = AV_PIX_FMT_NV12;
				if (!(e->hw_frames = make_frames(hw_dev, hf, in, w, h)))
					return AVERROR(EINVAL);
				c->hw_frames_ctx = av_buffer_ref(e->hw_frames);
				c->pix_fmt = hf;
				if (!(e->up = av_frame_alloc()))
					return AVERROR(ENOMEM);
			} else {
				c->pix_fmt = in;
			}
			e->sws = sws_getContext(src_w, src_h, AV_PIX_FMT_BGR0, w, h, in, SWS_BILINEAR, NULL, NULL, NULL);
			if (!e->sws)
				return AVERROR(EINVAL);
			const int *coef = sws_getCoefficients(SWS_CS_ITU709);
			sws_setColorspaceDetails(e->sws, coef, 1, coef, 0, 0, 1 << 16, 1 << 16);
			if (!(e->conv = av_frame_alloc()))
				return AVERROR(ENOMEM);
			e->conv->format = in;
			e->conv->width = w;
			e->conv->height = h;
			if ((ret = av_frame_get_buffer(e->conv, 0)) < 0)
				return ret;
		} else {
			c->pix_fmt = in;
		}
		if (!(e->cpu_pool = av_buffer_pool_init(src_w * src_h * 4, NULL)))
			return AVERROR(ENOMEM);
	}
	if ((ret = avcodec_open2(c, e->codec, opts)) < 0)
		return ret;
	return 0;
}

// cap_enc_frame returns a frame for the caller to fill: a hw surface from the
// encoder's pool, or a tight bgr0 buffer.
AVFrame *cap_enc_frame(cap_enc *e) {
	AVFrame *f = av_frame_alloc();
	if (!f)
		return NULL;
	if (e->src_fmt != AV_PIX_FMT_BGR0) {
		if (av_hwframe_get_buffer(e->hw_frames, f, 0) < 0)
			av_frame_free(&f);
		return f;
	}
	f->format = e->cpu_fmt;
	f->width = e->src_w;
	f->height = e->src_h;
	if (!(f->buf[0] = av_buffer_pool_get(e->cpu_pool))) {
		av_frame_free(&f);
		return NULL;
	}
	f->data[0] = f->buf[0]->data;
	f->linesize[0] = e->src_w * 4;
	return f;
}

// cap_enc_send takes ownership of f.
int cap_enc_send(cap_enc *e, AVFrame *f) {
	int ret;
	AVFrame *in = f;
	if (e->sws) {
		if ((ret = av_frame_make_writable(e->conv)) < 0)
			goto out;
		sws_scale(e->sws, (const uint8_t *const *)f->data, f->linesize, 0, e->src_h, e->conv->data, e->conv->linesize);
		e->conv->pts = f->pts;
		in = e->conv;
		if (e->up) {
			av_frame_unref(e->up);
			if ((ret = av_hwframe_get_buffer(e->hw_frames, e->up, 0)) < 0)
				goto out;
			if ((ret = av_hwframe_transfer_data(e->up, e->conv, 0)) < 0)
				goto out;
			e->up->pts = f->pts;
			in = e->up;
		}
	}
	if (e->force_key) {
		in->pict_type = AV_PICTURE_TYPE_I;
#ifdef AV_FRAME_FLAG_KEY
		in->flags |= AV_FRAME_FLAG_KEY;
#endif
		e->force_key = 0;
	} else {
		in->pict_type = AV_PICTURE_TYPE_NONE;
#ifdef AV_FRAME_FLAG_KEY
		in->flags &= ~AV_FRAME_FLAG_KEY;
#endif
	}
	ret = avcodec_send_frame(e->ctx, in);
out:
	av_frame_free(&f);
	return ret;
}

// cap_enc_recv: 1 = packet in e->pkt, 0 = needs more input, <0 error.
int cap_enc_recv(cap_enc *e) {
	av_packet_unref(e->pkt);
	int ret = avcodec_receive_packet(e->ctx, e->pkt);
	if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF)
		return 0;
	return ret < 0 ? ret : 1;
}

void cap_enc_close(cap_enc *e) {
	avcodec_free_context(&e->ctx);
	av_buffer_unref(&e->hw_frames);
	av_buffer_pool_uninit(&e->cpu_pool);
	sws_freeContext(e->sws);
	av_frame_free(&e->conv);
	av_frame_free(&e->up);
	av_packet_free(&e->pkt);
	memset(e, 0, sizeof *e);
}
