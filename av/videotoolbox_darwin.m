#import <VideoToolbox/VideoToolbox.h>
#import <CoreVideo/CoreVideo.h>
#include "videotoolbox_darwin.h"

// One session is enough: transfers are serial on the capture thread.
static VTPixelTransferSessionRef g_transfer;

int cap_vt_transfer(void *dstpb, void *srcpb) {
    if (!g_transfer) {
        VTPixelTransferSessionCreate(NULL, &g_transfer);
        if (!g_transfer)
            return -1;
    }
    OSStatus st = VTPixelTransferSessionTransferImage(g_transfer,
        (CVPixelBufferRef)srcpb, (CVPixelBufferRef)dstpb);
    return st == noErr ? 0 : (int)st;
}
