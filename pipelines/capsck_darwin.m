#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#include "capsck_darwin.h"

// CapSCKStream keeps only the newest frame: realtime consumers want
// latency, not backlog. SCStream's own queueDepth drops for it too.
@interface CapSCKStream : NSObject <SCStreamOutput, SCStreamDelegate>
@property(nonatomic, retain) SCStream *stream;
@property(nonatomic) dispatch_queue_t queue;
@property(nonatomic, retain) NSCondition *cond;
@property(nonatomic) CVPixelBufferRef latest;
@property(nonatomic, retain) NSError *err;
@property(nonatomic) BOOL stopped;
@end

@implementation CapSCKStream

- (void)stream:(SCStream *)stream
    didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
                   ofType:(SCStreamOutputType)type {
    if (type != SCStreamOutputTypeScreen || !CMSampleBufferIsValid(sampleBuffer))
        return;
    CVPixelBufferRef pb = CMSampleBufferGetImageBuffer(sampleBuffer);
    if (!pb)
        return;
    CVBufferRetain(pb);
    [_cond lock];
    if (_latest)
        CVBufferRelease(_latest);
    _latest = pb;
    [_cond signal];
    [_cond unlock];
}

- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
    [_cond lock];
    _stopped = YES;
    if (!_err)
        _err = [error retain];
    [_cond broadcast];
    [_cond unlock];
}

@end

static void cap_sck_err(char *errbuf, int errlen, NSString *fmt, ...) {
    if (!errbuf || errlen <= 0)
        return;
    va_list ap;
    va_start(ap, fmt);
    NSString *s = [[NSString alloc] initWithFormat:fmt arguments:ap];
    va_end(ap);
    snprintf(errbuf, errlen, "%s", [s UTF8String]);
    [s release];
}

void *cap_sck_start(uint32_t displayID, int w, int h, int fps, char *errbuf, int errlen) {
    @autoreleasepool {
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        __block SCShareableContent *content = nil;
        __block NSError *serr = nil;
        [SCShareableContent getShareableContentWithCompletionHandler:^(SCShareableContent *c, NSError *e) {
            content = [c retain];
            serr = [e retain];
            dispatch_semaphore_signal(sem);
        }];
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        if (serr || !content) {
            cap_sck_err(errbuf, errlen, @"shareable content: %@", serr ? serr : @"empty");
            [serr release];
            return NULL;
        }
        SCDisplay *display = nil;
        for (SCDisplay *d in content.displays)
            if (d.displayID == displayID)
                display = d;
        if (!display) {
            cap_sck_err(errbuf, errlen, @"display %u not found", displayID);
            [content release];
            return NULL;
        }

        SCStreamConfiguration *cfg = [[SCStreamConfiguration alloc] init];
        cfg.width = w;
        cfg.height = h;
        cfg.minimumFrameInterval = CMTimeMake(1, fps);
        cfg.pixelFormat = kCVPixelFormatType_32BGRA;
        cfg.queueDepth = 4;
        cfg.showsCursor = YES;
        SCContentFilter *filter =
            [[SCContentFilter alloc] initWithDisplay:display excludingWindows:@[]];

        CapSCKStream *s = [[CapSCKStream alloc] init];
        s.cond = [[NSCondition alloc] init];
        s.queue = dispatch_queue_create("captured.sck", DISPATCH_QUEUE_SERIAL);
        s.stream = [[SCStream alloc] initWithFilter:filter
                                    configuration:cfg
                                         delegate:s];
        [cfg release];
        [filter release];
        [content release];
        if (!s.stream) {
            cap_sck_err(errbuf, errlen, @"SCStream init failed");
            [s release];
            return NULL;
        }
        NSError *aerr = nil;
        if (![s.stream addStreamOutput:s
                                  type:SCStreamOutputTypeScreen
                    sampleHandlerQueue:s.queue
                                 error:&aerr]) {
            cap_sck_err(errbuf, errlen, @"addStreamOutput: %@", aerr);
            [s release];
            return NULL;
        }
        __block NSError *sstartErr = nil;
        [s.stream startCaptureWithCompletionHandler:^(NSError *e) {
            sstartErr = [e retain];
            dispatch_semaphore_signal(sem);
        }];
        dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
        if (sstartErr) {
            cap_sck_err(errbuf, errlen, @"startCapture: %@", sstartErr);
            [sstartErr release];
            [s release];
            return NULL;
        }
        return (void *)s;
    }
}

int cap_sck_next(void *h, int timeout_ms, void **out, char *errbuf, int errlen) {
    CapSCKStream *s = (CapSCKStream *)h;
    *out = NULL;
    [s.cond lock];
    while (!s.latest && !s.stopped && !s.err) {
        NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:timeout_ms / 1000.0];
        if (![s.cond waitUntilDate:deadline]) {
            [s.cond unlock];
            return 1;
        }
    }
    if (!s.latest) {
        NSError *e = s.err;
        if (!e && s.stopped)
            e = [NSError errorWithDomain:@"captured" code:1
                                userInfo:@{NSLocalizedDescriptionKey : @"stream stopped"}];
        cap_sck_err(errbuf, errlen, @"%@", e ? e : @"no frame");
        [s.cond unlock];
        return -1;
    }
    *out = s.latest;
    s.latest = NULL;
    [s.cond unlock];
    return 0;
}

int cap_sck_pb_copy(void *pb_, void *dst, int w, int h) {
    CVPixelBufferRef pb = (CVPixelBufferRef)pb_;
    if (CVPixelBufferGetPixelFormatType(pb) != kCVPixelFormatType_32BGRA)
        return -1;
    if ((int)CVPixelBufferGetWidth(pb) != w || (int)CVPixelBufferGetHeight(pb) != h)
        return -2;
    CVPixelBufferLockBaseAddress(pb, kCVPixelBufferLock_ReadOnly);
    uint8_t *src = (uint8_t *)CVPixelBufferGetBaseAddress(pb);
    size_t stride = CVPixelBufferGetBytesPerRow(pb);
    uint8_t *d = (uint8_t *)dst;
    for (int y = 0; y < h; y++)
        memcpy(d + (size_t)y * w * 4, src + (size_t)y * stride, (size_t)w * 4);
    CVPixelBufferUnlockBaseAddress(pb, kCVPixelBufferLock_ReadOnly);
    return 0;
}

void cap_sck_pb_release(void *pb) { CVBufferRelease((CVPixelBufferRef)pb); }

void cap_sck_pb_size(void *pb, int *w, int *h) {
    *w = (int)CVPixelBufferGetWidth((CVPixelBufferRef)pb);
    *h = (int)CVPixelBufferGetHeight((CVPixelBufferRef)pb);
}

void cap_sck_stop(void *h) {
    CapSCKStream *s = (CapSCKStream *)h;
    if (!s)
        return;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    [s.stream stopCaptureWithCompletionHandler:^(NSError *e) {
        dispatch_semaphore_signal(sem);
    }];
    dispatch_semaphore_wait(sem, DISPATCH_TIME_FOREVER);
    [s.cond lock];
    s.stopped = YES;
    [s.cond broadcast];
    [s.cond unlock];
    [s.cond release];
    [s.stream release];
    if (s.latest)
        CVBufferRelease(s.latest);
    [s.err release];
    [s release];
}
