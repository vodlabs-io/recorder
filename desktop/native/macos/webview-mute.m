// Page-level audio mute for the app's hidden platform webviews (macOS).
//
// The hidden views load platform pages (TikTok's feed, for one) that autoplay
// with sound. vl_webview_mute mutes the whole page through WebKit's own mute
// state, which also covers Web Audio and media in cross-origin iframes. Built
// into libVLLoginPopups.dylib by scripts/build-macos-native.ts.

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

// _WKMediaAudioMuted from WebKit's _WKMediaMutedState.
static const NSUInteger kAudioMuted = 1 << 0;

static const int kMuteTries = 100;
static const int64_t kMuteRetryNs = 50 * NSEC_PER_MSEC;

// Electrobun builds the WKWebView asynchronously after the view's pointer
// exists, so this retries until it does.
static void mute(id impl, int tries) {
    if (![impl respondsToSelector:@selector(webView)]) return; // not a WKWebView-backed view
    WKWebView *webView = [impl valueForKey:@"webView"];
    if (![webView isKindOfClass:WKWebView.class]) {
        if (tries < kMuteTries) {
            dispatch_after(dispatch_time(DISPATCH_TIME_NOW, kMuteRetryNs), dispatch_get_main_queue(), ^{
                mute(impl, tries + 1);
            });
        }
        return;
    }
    SEL setMuted = NSSelectorFromString(@"_setPageMuted:");
    if (![webView respondsToSelector:setMuted]) {
        NSLog(@"[webview-mute] WKWebView has no _setPageMuted:");
        return;
    }
    ((void (*)(id, SEL, NSUInteger))[webView methodForSelector:setMuted])(webView, setMuted, kAudioMuted);
}

// view: an Electrobun BrowserView's native pointer (BrowserView.ptr).
__attribute__((visibility("default"))) void vl_webview_mute(void *view) {
    id impl = (__bridge id)view;
    dispatch_async(dispatch_get_main_queue(), ^{
        mute(impl, 0);
    });
}
