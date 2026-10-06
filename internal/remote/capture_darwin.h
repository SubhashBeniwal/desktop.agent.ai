// Native macOS screen capture (ScreenCaptureKit), H.264 encoding
// (VideoToolbox) and input injection (CGEvent / Accessibility) for the remote
// package. Strings returned as char* are malloc'd; the caller frees them.
#pragma once
#include <stdint.h>

// Permissions.
int aio_preflight_screen(void);
int aio_preflight_accessibility(void);
void aio_request_screen(void);
void aio_request_accessibility(void);

// JSON {"windows":[...],"displays":[...]}; NULL with *err set on failure.
char *aio_list_json(char **err);

// Current geometry in global points (top-left origin). Returns 0 if the
// window is gone or not on screen.
int aio_window_bounds(uint32_t window_id, double *x, double *y, double *w, double *h, int *pid);
int aio_display_bounds(uint32_t display_id, double *x, double *y, double *w, double *h);
// Pixels per point of the display containing (x, y).
double aio_backing_scale(double x, double y);

// Capture + encode. Frames are delivered to the Go export aioGoFrame as Annex-B
// H.264 access units tagged with handle. Returns an opaque capture or NULL.
void *aio_capture_start(uintptr_t handle, uint32_t window_id, uint32_t display_id,
                        int width, int height, int fps, int kbps, char **err);
void aio_capture_keyframe(void *cap);
int aio_capture_resize(void *cap, int width, int height, char **err);
void aio_capture_stop(void *cap);

// Input. Points are global, top-left origin.
// kind: 0 move, 1 down, 2 up. button: 0 left, 1 right, 2 middle.
void aio_mouse(int kind, int button, double x, double y, int clicks, int dragging);
void aio_scroll(double x, double y, int dx, int dy);
void aio_key(uint16_t keycode, int down, uint64_t flags);
void aio_text(const uint16_t *utf16, int n);
// Activate the app and raise its window matching the given frame.
void aio_raise(int pid, double x, double y, double w, double h);
