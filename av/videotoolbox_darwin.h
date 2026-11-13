#ifndef CAPTURED_VIDEOTOOLBOX_DARWIN_H
#define CAPTURED_VIDEOTOOLBOX_DARWIN_H

// cap_vt_transfer scales/copies a CVPixelBuffer into another on the GPU.
// Returns 0 or an OSStatus.
int cap_vt_transfer(void *dstpb, void *srcpb);

#endif
