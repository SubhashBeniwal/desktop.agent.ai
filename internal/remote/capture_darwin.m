//go:build darwin && cgo

#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#import <CoreMedia/CoreMedia.h>
#import <Foundation/Foundation.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <VideoToolbox/VideoToolbox.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

#include "capture_darwin.h"

// Implemented in Go (platform_darwin.go).
extern void aioGoFrame(uintptr_t handle, void *data, int len, int64_t pts_us, int keyframe);
extern void aioGoStopped(uintptr_t handle, char *reason);

static char *dupstr(NSString *s) { return strdup(s ? s.UTF8String : ""); }

// ---------------------------------------------------------------------------
// Permissions

int aio_preflight_screen(void) { return CGPreflightScreenCaptureAccess() ? 1 : 0; }
int aio_preflight_accessibility(void) { return AXIsProcessTrusted() ? 1 : 0; }
void aio_request_screen(void) { CGRequestScreenCaptureAccess(); }
void aio_request_accessibility(void) {
    NSDictionary *opts = @{(__bridge id)kAXTrustedCheckOptionPrompt : @YES};
    AXIsProcessTrustedWithOptions((__bridge CFDictionaryRef)opts);
}

// ---------------------------------------------------------------------------
// Shareable content

static SCShareableContent *shareable(BOOL onScreenOnly, NSString **err) {
    __block SCShareableContent *content = nil;
    __block NSError *error = nil;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    [SCShareableContent getShareableContentExcludingDesktopWindows:YES
                                               onScreenWindowsOnly:onScreenOnly
                                                 completionHandler:^(SCShareableContent *c, NSError *e) {
                                                   content = c;
                                                   error = e;
                                                   dispatch_semaphore_signal(sem);
                                                 }];
    if (dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC)) != 0) {
        *err = @"timed out listing shareable content";
        return nil;
    }
    if (error) {
        *err = error.localizedDescription;
        return nil;
    }
    return content;
}

char *aio_list_json(char **err) {
    @autoreleasepool {
        NSString *e = nil;
        SCShareableContent *content = shareable(NO, &e);
        if (!content) {
            *err = dupstr(e);
            return NULL;
        }
        pid_t self = getpid();
        NSMutableArray *windows = [NSMutableArray array];
        for (SCWindow *w in content.windows) {
            SCRunningApplication *app = w.owningApplication;
            // Normal app windows only: layer 0, reasonably sized, and either
            // visible or titled (skips hidden helper windows).
            if (!app || w.windowLayer != 0 || app.processID == self) continue;
            if (w.frame.size.width < 50 || w.frame.size.height < 50) continue;
            if (!w.onScreen && w.title.length == 0) continue;
            [windows addObject:@{
                @"id" : @(w.windowID),
                @"app" : app.applicationName ?: @"",
                @"bundle_id" : app.bundleIdentifier ?: @"",
                @"pid" : @(app.processID),
                @"title" : w.title ?: @"",
                @"width" : @((int)w.frame.size.width),
                @"height" : @((int)w.frame.size.height),
                @"on_screen" : @(w.onScreen),
            }];
        }
        NSMutableArray *displays = [NSMutableArray array];
        for (SCDisplay *d in content.displays) {
            [displays addObject:@{@"id" : @(d.displayID), @"width" : @(d.width), @"height" : @(d.height)}];
        }
        NSData *json = [NSJSONSerialization dataWithJSONObject:@{@"windows" : windows, @"displays" : displays}
                                                       options:0
                                                         error:nil];
        char *out = malloc(json.length + 1);
        memcpy(out, json.bytes, json.length);
        out[json.length] = 0;
        return out;
    }
}

// ---------------------------------------------------------------------------
// Geometry

int aio_window_bounds(uint32_t window_id, double *x, double *y, double *w, double *h, int *pid) {
    CFArrayRef list = CGWindowListCopyWindowInfo(kCGWindowListOptionIncludingWindow, window_id);
    if (!list) return 0;
    int ok = 0;
    if (CFArrayGetCount(list) > 0) {
        NSDictionary *info = (__bridge NSDictionary *)CFArrayGetValueAtIndex(list, 0);
        CGRect r;
        if ([info[(id)kCGWindowIsOnscreen] boolValue] &&
            CGRectMakeWithDictionaryRepresentation((__bridge CFDictionaryRef)info[(id)kCGWindowBounds], &r)) {
            *x = r.origin.x, *y = r.origin.y, *w = r.size.width, *h = r.size.height;
            *pid = [info[(id)kCGWindowOwnerPID] intValue];
            ok = 1;
        }
    }
    CFRelease(list);
    return ok;
}

int aio_display_bounds(uint32_t display_id, double *x, double *y, double *w, double *h) {
    CGRect r = CGDisplayBounds(display_id);
    if (CGRectIsEmpty(r)) return 0;
    *x = r.origin.x, *y = r.origin.y, *w = r.size.width, *h = r.size.height;
    return 1;
}

double aio_backing_scale(double x, double y) {
    CGDirectDisplayID id;
    uint32_t n = 0;
    if (CGGetDisplaysWithPoint(CGPointMake(x, y), 1, &id, &n) != kCGErrorSuccess || n == 0) {
        id = CGMainDisplayID();
    }
    CGDisplayModeRef mode = CGDisplayCopyDisplayMode(id);
    if (!mode) return 1;
    double scale = (double)CGDisplayModeGetPixelWidth(mode) / (double)CGDisplayModeGetWidth(mode);
    CGDisplayModeRelease(mode);
    return scale > 0 ? scale : 1;
}

// ---------------------------------------------------------------------------
// Capture + encode

@interface AIOCapture : NSObject <SCStreamOutput, SCStreamDelegate>
@end

@implementation AIOCapture {
  @public
    uintptr_t _handle;
    SCStream *_stream;
    dispatch_queue_t _queue;
    VTCompressionSessionRef _vt;
    CVPixelBufferRef _last; // last complete frame, re-encoded on keyframe requests
    int _width, _height, _fps, _kbps;
    atomic_bool _forceKey;
}

static void vtOutput(void *refcon, void *frameRefcon, OSStatus status, VTEncodeInfoFlags flags,
                     CMSampleBufferRef sb) {
    if (status != noErr || !sb || !CMSampleBufferDataIsReady(sb)) return;
    AIOCapture *cap = (__bridge AIOCapture *)refcon;

    BOOL key = YES;
    CFArrayRef atts = CMSampleBufferGetSampleAttachmentsArray(sb, false);
    if (atts && CFArrayGetCount(atts) > 0) {
        CFDictionaryRef a = CFArrayGetValueAtIndex(atts, 0);
        key = !CFDictionaryContainsKey(a, kCMSampleAttachmentKey_NotSync);
    }

    NSMutableData *out = [NSMutableData data];
    static const uint8_t startCode[4] = {0, 0, 0, 1};
    int nalLen = 4;

    CMFormatDescriptionRef fmt = CMSampleBufferGetFormatDescription(sb);
    size_t count = 0;
    CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, 0, NULL, NULL, &count, &nalLen);
    if (key) {
        // Prefix keyframes with SPS/PPS so a decoder can start here.
        for (size_t i = 0; i < count; i++) {
            const uint8_t *ps;
            size_t psLen;
            if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fmt, i, &ps, &psLen, NULL, NULL) == noErr) {
                [out appendBytes:startCode length:4];
                [out appendBytes:ps length:psLen];
            }
        }
    }

    // AVCC (length-prefixed) -> Annex B (start-code-prefixed).
    CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sb);
    size_t total = CMBlockBufferGetDataLength(block);
    uint8_t *buf = malloc(total);
    if (CMBlockBufferCopyDataBytes(block, 0, total, buf) == kCMBlockBufferNoErr) {
        size_t off = 0;
        while (off + nalLen <= total) {
            uint32_t n = 0;
            for (int i = 0; i < nalLen; i++) n = (n << 8) | buf[off + i];
            off += nalLen;
            if (off + n > total) break;
            [out appendBytes:startCode length:4];
            [out appendBytes:buf + off length:n];
            off += n;
        }
    }
    free(buf);

    CMTime pts = CMSampleBufferGetPresentationTimeStamp(sb);
    int64_t us = (int64_t)(CMTimeGetSeconds(pts) * 1e6);
    aioGoFrame(cap->_handle, (void *)out.bytes, (int)out.length, us, key ? 1 : 0);
}

- (BOOL)makeEncoder:(NSString **)err {
    NSDictionary *spec = @{(__bridge id)kVTVideoEncoderSpecification_EnableLowLatencyRateControl : @YES};
    OSStatus st = VTCompressionSessionCreate(NULL, _width, _height, kCMVideoCodecType_H264,
                                             (__bridge CFDictionaryRef)spec, NULL, NULL, vtOutput,
                                             (__bridge void *)self, &_vt);
    if (st != noErr) { // fall back to the default (non low-latency) encoder
        st = VTCompressionSessionCreate(NULL, _width, _height, kCMVideoCodecType_H264, NULL, NULL, NULL,
                                        vtOutput, (__bridge void *)self, &_vt);
    }
    if (st != noErr) {
        *err = [NSString stringWithFormat:@"VTCompressionSessionCreate failed: %d", (int)st];
        return NO;
    }
    VTSessionSetProperty(_vt, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue);
    VTSessionSetProperty(_vt, kVTCompressionPropertyKey_ProfileLevel,
                         kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel);
    VTSessionSetProperty(_vt, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse);
    VTSessionSetProperty(_vt, kVTCompressionPropertyKey_AverageBitRate, (__bridge CFNumberRef) @(_kbps * 1000));
    VTSessionSetProperty(_vt, kVTCompressionPropertyKey_ExpectedFrameRate, (__bridge CFNumberRef) @(_fps));
    VTSessionSetProperty(_vt, kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, (__bridge CFNumberRef) @(5));
    VTCompressionSessionPrepareToEncodeFrames(_vt);
    return YES;
}

- (void)dropEncoder {
    if (_vt) {
        VTCompressionSessionCompleteFrames(_vt, kCMTimeInvalid);
        VTCompressionSessionInvalidate(_vt);
        CFRelease(_vt);
        _vt = NULL;
    }
}

// Must run on _queue.
- (void)encode:(CVPixelBufferRef)img pts:(CMTime)pts {
    if (!_vt) return;
    // Frames of the old size can still arrive briefly after a resize.
    if ((int)CVPixelBufferGetWidth(img) != _width || (int)CVPixelBufferGetHeight(img) != _height) return;
    NSDictionary *opts = nil;
    if (atomic_exchange(&_forceKey, false)) {
        opts = @{(__bridge id)kVTEncodeFrameOptionKey_ForceKeyFrame : @YES};
    }
    VTCompressionSessionEncodeFrame(_vt, img, pts, kCMTimeInvalid, (__bridge CFDictionaryRef)opts, NULL, NULL);
}

- (void)stream:(SCStream *)stream didOutputSampleBuffer:(CMSampleBufferRef)sb ofType:(SCStreamOutputType)type {
    if (type != SCStreamOutputTypeScreen || !CMSampleBufferIsValid(sb)) return;
    CFArrayRef atts = CMSampleBufferGetSampleAttachmentsArray(sb, false);
    if (!atts || CFArrayGetCount(atts) == 0) return;
    NSDictionary *a = (__bridge NSDictionary *)CFArrayGetValueAtIndex(atts, 0);
    NSNumber *status = a[SCStreamFrameInfoStatus];
    // Idle/blank frames carry no new image.
    if (!status || status.integerValue != SCFrameStatusComplete) return;
    CVPixelBufferRef img = CMSampleBufferGetImageBuffer(sb);
    if (!img) return;
    if (_last) CVPixelBufferRelease(_last);
    _last = CVPixelBufferRetain(img);
    [self encode:img pts:CMSampleBufferGetPresentationTimeStamp(sb)];
}

- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
    aioGoStopped(_handle, dupstr(error.localizedDescription));
}

- (SCStreamConfiguration *)config {
    SCStreamConfiguration *c = [SCStreamConfiguration new];
    c.width = _width;
    c.height = _height;
    c.minimumFrameInterval = CMTimeMake(1, _fps);
    c.pixelFormat = kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange;
    c.showsCursor = YES;
    c.queueDepth = 5;
    return c;
}

@end

void *aio_capture_start(uintptr_t handle, uint32_t window_id, uint32_t display_id, int width, int height,
                        int fps, int kbps, char **err) {
    @autoreleasepool {
        NSString *e = nil;
        SCShareableContent *content = shareable(YES, &e);
        if (!content) {
            *err = dupstr(e);
            return NULL;
        }
        SCContentFilter *filter = nil;
        if (window_id) {
            for (SCWindow *w in content.windows) {
                if (w.windowID == window_id) {
                    filter = [[SCContentFilter alloc] initWithDesktopIndependentWindow:w];
                    break;
                }
            }
        } else {
            for (SCDisplay *d in content.displays) {
                if (d.displayID == display_id) {
                    filter = [[SCContentFilter alloc] initWithDisplay:d excludingWindows:@[]];
                    break;
                }
            }
        }
        if (!filter) {
            *err = dupstr(@"not found");
            return NULL;
        }

        AIOCapture *cap = [AIOCapture new];
        cap->_handle = handle;
        cap->_width = width, cap->_height = height, cap->_fps = fps, cap->_kbps = kbps;
        atomic_store(&cap->_forceKey, true);
        cap->_queue = dispatch_queue_create("aio.capture", DISPATCH_QUEUE_SERIAL);
        if (![cap makeEncoder:&e]) {
            *err = dupstr(e);
            return NULL;
        }

        cap->_stream = [[SCStream alloc] initWithFilter:filter configuration:[cap config] delegate:cap];
        NSError *addErr = nil;
        if (![cap->_stream addStreamOutput:cap
                                       type:SCStreamOutputTypeScreen
                         sampleHandlerQueue:cap->_queue
                                      error:&addErr]) {
            [cap dropEncoder];
            *err = dupstr(addErr.localizedDescription);
            return NULL;
        }

        __block NSError *startErr = nil;
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);
        [cap->_stream startCaptureWithCompletionHandler:^(NSError *e2) {
          startErr = e2;
          dispatch_semaphore_signal(sem);
        }];
        dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC));
        if (startErr) {
            [cap dropEncoder];
            *err = dupstr(startErr.localizedDescription);
            return NULL;
        }
        return (__bridge_retained void *)cap;
    }
}

void aio_capture_keyframe(void *p) {
    AIOCapture *cap = (__bridge AIOCapture *)p;
    atomic_store(&cap->_forceKey, true);
    // A static window produces no new frames, so re-encode the last one.
    dispatch_async(cap->_queue, ^{
      if (cap->_last) [cap encode:cap->_last pts:CMClockGetTime(CMClockGetHostTimeClock())];
    });
}

int aio_capture_resize(void *p, int width, int height, char **err) {
    AIOCapture *cap = (__bridge AIOCapture *)p;
    __block NSString *e = nil;
    dispatch_sync(cap->_queue, ^{
      [cap dropEncoder];
      if (cap->_last) {
          CVPixelBufferRelease(cap->_last);
          cap->_last = NULL;
      }
      cap->_width = width, cap->_height = height;
      atomic_store(&cap->_forceKey, true);
      [cap makeEncoder:&e];
    });
    if (e) {
        *err = dupstr(e);
        return 0;
    }
    __block NSError *upErr = nil;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    [cap->_stream updateConfiguration:[cap config]
                    completionHandler:^(NSError *e2) {
                      upErr = e2;
                      dispatch_semaphore_signal(sem);
                    }];
    dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC));
    if (upErr) {
        *err = dupstr(upErr.localizedDescription);
        return 0;
    }
    return 1;
}

void aio_capture_stop(void *p) {
    AIOCapture *cap = (__bridge_transfer AIOCapture *)p;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    [cap->_stream stopCaptureWithCompletionHandler:^(NSError *e) {
      dispatch_semaphore_signal(sem);
    }];
    dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, 3 * NSEC_PER_SEC));
    dispatch_sync(cap->_queue, ^{
      [cap dropEncoder];
      if (cap->_last) {
          CVPixelBufferRelease(cap->_last);
          cap->_last = NULL;
      }
    });
}

// ---------------------------------------------------------------------------
// Input

static void post(CGEventRef ev) {
    if (!ev) return;
    CGEventPost(kCGHIDEventTap, ev);
    CFRelease(ev);
}

void aio_mouse(int kind, int button, double x, double y, int clicks, int dragging) {
    static const CGEventType down[] = {kCGEventLeftMouseDown, kCGEventRightMouseDown, kCGEventOtherMouseDown};
    static const CGEventType up[] = {kCGEventLeftMouseUp, kCGEventRightMouseUp, kCGEventOtherMouseUp};
    static const CGEventType drag[] = {kCGEventLeftMouseDragged, kCGEventRightMouseDragged,
                                       kCGEventOtherMouseDragged};
    static const CGMouseButton btn[] = {kCGMouseButtonLeft, kCGMouseButtonRight, kCGMouseButtonCenter};
    if (button < 0 || button > 2) button = 0;

    CGEventType type;
    switch (kind) {
    case 1: type = down[button]; break;
    case 2: type = up[button]; break;
    default: type = dragging ? drag[button] : kCGEventMouseMoved;
    }
    CGEventRef ev = CGEventCreateMouseEvent(NULL, type, CGPointMake(x, y), btn[button]);
    if (kind != 0 && clicks > 0) CGEventSetIntegerValueField(ev, kCGMouseEventClickState, clicks);
    post(ev);
}

void aio_scroll(double x, double y, int dx, int dy) {
    // Scroll goes to the window under the cursor, so move there first.
    post(CGEventCreateMouseEvent(NULL, kCGEventMouseMoved, CGPointMake(x, y), kCGMouseButtonLeft));
    // Browser convention: dy > 0 scrolls content down == wheel "towards user".
    CGEventRef ev = CGEventCreateScrollWheelEvent2(NULL, kCGScrollEventUnitPixel, 2, -dy, -dx, 0);
    CGEventSetLocation(ev, CGPointMake(x, y));
    post(ev);
}

void aio_key(uint16_t keycode, int down, uint64_t flags) {
    CGEventRef ev = CGEventCreateKeyboardEvent(NULL, keycode, down ? true : false);
    CGEventSetFlags(ev, (CGEventFlags)flags);
    post(ev);
}

void aio_text(const uint16_t *utf16, int n) {
    // CGEventKeyboardSetUnicodeString reliably handles short chunks only.
    for (int off = 0; off < n; off += 16) {
        int len = n - off < 16 ? n - off : 16;
        for (int down = 1; down >= 0; down--) {
            CGEventRef ev = CGEventCreateKeyboardEvent(NULL, 0, down ? true : false);
            CGEventSetFlags(ev, 0);
            CGEventKeyboardSetUnicodeString(ev, len, utf16 + off);
            post(ev);
        }
    }
}

void aio_raise(int pid, double x, double y, double w, double h) {
    NSRunningApplication *app = [NSRunningApplication runningApplicationWithProcessIdentifier:pid];
    if (!app) return;
    if (!app.active) {
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
        [app activateWithOptions:NSApplicationActivateIgnoringOtherApps];
#pragma clang diagnostic pop
    }
    // Raise the specific window: match the AX window by frame.
    AXUIElementRef axApp = AXUIElementCreateApplication(pid);
    CFArrayRef wins = NULL;
    if (AXUIElementCopyAttributeValue(axApp, kAXWindowsAttribute, (CFTypeRef *)&wins) == kAXErrorSuccess && wins) {
        for (CFIndex i = 0; i < CFArrayGetCount(wins); i++) {
            AXUIElementRef win = CFArrayGetValueAtIndex(wins, i);
            CFTypeRef posV = NULL, sizeV = NULL;
            CGPoint pos;
            CGSize size;
            BOOL match = NO;
            if (AXUIElementCopyAttributeValue(win, kAXPositionAttribute, &posV) == kAXErrorSuccess &&
                AXUIElementCopyAttributeValue(win, kAXSizeAttribute, &sizeV) == kAXErrorSuccess &&
                AXValueGetValue(posV, kAXValueCGPointType, &pos) && AXValueGetValue(sizeV, kAXValueCGSizeType, &size)) {
                match = fabs(pos.x - x) < 2 && fabs(pos.y - y) < 2 && fabs(size.width - w) < 2 &&
                        fabs(size.height - h) < 2;
            }
            if (posV) CFRelease(posV);
            if (sizeV) CFRelease(sizeV);
            if (match) {
                AXUIElementPerformAction(win, kAXRaiseAction);
                AXUIElementSetAttributeValue(win, kAXMainAttribute, kCFBooleanTrue);
                break;
            }
        }
        CFRelease(wins);
    }
    CFRelease(axApp);
}
