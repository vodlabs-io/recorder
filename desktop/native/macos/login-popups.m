// Popup windows for platform sign-in views (macOS).
//
// Electrobun's WKUIDelegate answers every window.open with no view, so a sign-in
// that runs in a popup (e.g. "Sign in with Google" through Google Identity
// Services) never starts. vl_login_popups_enable gives one webview a delegate
// that opens each popup as a real window built from the configuration WebKit
// hands over, so the popup shares the page's partition and keeps window.opener,
// which such sign-ins report back through. Every other delegate call goes to
// Electrobun's delegate. Built into the app bundle by scripts/build-macos-native.ts.

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <objc/runtime.h>

static char kDelegateKey;

// Popups stay open until their window closes; this holds them until then.
static NSMutableSet<NSWindow *> *openPopups;

static const CGFloat kDefaultWidth = 500;
static const CGFloat kDefaultHeight = 640;

@interface VLPopupDelegate : NSObject <WKUIDelegate, NSWindowDelegate>
// Electrobun's delegate for the sign-in view; nil for a popup.
@property (nonatomic, strong) id<WKUIDelegate> inner;
// The popup's own window; nil for the sign-in view.
@property (nonatomic, weak) NSWindow *popupWindow;
// Observes the opener's window closing, which closes the popup with it.
@property (nonatomic, strong) id openerCloseObserver;
@end

@implementation VLPopupDelegate

- (BOOL)respondsToSelector:(SEL)sel {
    return [super respondsToSelector:sel] || [self.inner respondsToSelector:sel];
}

- (id)forwardingTargetForSelector:(SEL)sel {
    return [self.inner respondsToSelector:sel] ? self.inner : nil;
}

- (WKWebView *)webView:(WKWebView *)webView
    createWebViewWithConfiguration:(WKWebViewConfiguration *)configuration
               forNavigationAction:(WKNavigationAction *)action
                    windowFeatures:(WKWindowFeatures *)features {
    CGFloat width = features.width ? features.width.doubleValue : kDefaultWidth;
    CGFloat height = features.height ? features.height.doubleValue : kDefaultHeight;
    NSRect frame = NSMakeRect(0, 0, width, height);

    NSWindow *win = [[NSWindow alloc]
        initWithContentRect:frame
                  styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable |
                            NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable
                    backing:NSBackingStoreBuffered
                      defer:NO];
    win.releasedWhenClosed = NO;
    win.title = action.request.URL.host ?: @"";

    // WebKit requires the returned view to use exactly this configuration.
    WKWebView *child = [[WKWebView alloc] initWithFrame:frame configuration:configuration];
    child.customUserAgent = webView.customUserAgent;
    child.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;

    VLPopupDelegate *delegate = [VLPopupDelegate new];
    delegate.popupWindow = win;
    child.UIDelegate = delegate;
    objc_setAssociatedObject(child, &kDelegateKey, delegate, OBJC_ASSOCIATION_RETAIN_NONATOMIC);
    win.contentView = child;
    win.delegate = delegate;

    NSWindow *opener = webView.window;
    if (opener) {
        NSRect of = opener.frame;
        [win setFrameOrigin:NSMakePoint(NSMidX(of) - width / 2, NSMidY(of) - height / 2)];
        __weak NSWindow *weakWin = win;
        delegate.openerCloseObserver = [NSNotificationCenter.defaultCenter
            addObserverForName:NSWindowWillCloseNotification
                        object:opener
                         queue:NSOperationQueue.mainQueue
                    usingBlock:^(__unused NSNotification *note) {
                        [weakWin close];
                    }];
    } else {
        [win center];
    }

    if (!openPopups) openPopups = [NSMutableSet set];
    [openPopups addObject:win];
    [win makeKeyAndOrderFront:nil];
    return child;
}

// window.close() from the page.
- (void)webViewDidClose:(WKWebView *)webView {
    if (self.popupWindow) {
        [self.popupWindow close];
    } else if ([self.inner respondsToSelector:_cmd]) {
        [self.inner webViewDidClose:webView];
    }
}

- (void)windowWillClose:(NSNotification *)note {
    if (self.openerCloseObserver) {
        [NSNotificationCenter.defaultCenter removeObserver:self.openerCloseObserver];
        self.openerCloseObserver = nil;
    }
    NSWindow *win = note.object;
    // Released after the close finishes, not from inside it.
    dispatch_async(dispatch_get_main_queue(), ^{
        [openPopups removeObject:win];
    });
}

@end

static const int kAttachTries = 100;
static const int64_t kAttachRetryNs = 50 * NSEC_PER_MSEC;

// Electrobun builds the WKWebView asynchronously after the view's pointer
// exists, so this retries until it does.
static void attach(id impl, int tries) {
    if (![impl respondsToSelector:@selector(webView)]) return; // not a WKWebView-backed view
    WKWebView *webView = [impl valueForKey:@"webView"];
    if (![webView isKindOfClass:WKWebView.class]) {
        if (tries < kAttachTries) {
            dispatch_after(dispatch_time(DISPATCH_TIME_NOW, kAttachRetryNs), dispatch_get_main_queue(), ^{
                attach(impl, tries + 1);
            });
        }
        return;
    }
    if ([webView.UIDelegate isKindOfClass:VLPopupDelegate.class]) return;

    VLPopupDelegate *delegate = [VLPopupDelegate new];
    delegate.inner = webView.UIDelegate;
    webView.UIDelegate = delegate;
    objc_setAssociatedObject(webView, &kDelegateKey, delegate, OBJC_ASSOCIATION_RETAIN_NONATOMIC);
}

// view: an Electrobun BrowserView's native pointer (BrowserView.ptr).
__attribute__((visibility("default"))) void vl_login_popups_enable(void *view) {
    id impl = (__bridge id)view;
    dispatch_async(dispatch_get_main_queue(), ^{
        attach(impl, 0);
    });
}
