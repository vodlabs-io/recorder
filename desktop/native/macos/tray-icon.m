// Tray icon swap that stays a template image (macOS).
//
// Electrobun's setTrayImage drops the template flag and the size createTray gave
// the icon, so a swapped icon stops following the menu bar's light/dark and shows
// at its pixel size. Electrobun doesn't expose its status item either, so this
// notes the one NSStatusBar hands out and sets the image on its button.
// Built into libVLLoginPopups.dylib by scripts/build-macos-native.ts.

#import <Cocoa/Cocoa.h>
#import <objc/runtime.h>

static IMP originalStatusItemWithLength;
static NSStatusItem *trayStatusItem;

static NSStatusItem *noteStatusItem(id self, SEL _cmd, CGFloat length) {
    NSStatusItem *item = ((NSStatusItem * (*)(id, SEL, CGFloat)) originalStatusItemWithLength)(self, _cmd, length);
    trayStatusItem = item;
    return item;
}

// Call before creating the Tray.
__attribute__((visibility("default"))) void vl_tray_watch_status_item(void) {
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        Method method = class_getInstanceMethod(NSStatusBar.class, @selector(statusItemWithLength:));
        originalStatusItemWithLength = method_setImplementation(method, (IMP)noteStatusItem);
    });
}

__attribute__((visibility("default"))) void vl_tray_set_template_image(const char *path, double width, double height) {
    NSString *file = [NSString stringWithUTF8String:path];
    dispatch_async(dispatch_get_main_queue(), ^{
        NSImage *image = [[NSImage alloc] initWithContentsOfFile:file];
        NSStatusBarButton *button = trayStatusItem.button;
        if (!image || !button) {
            NSLog(@"[tray-icon] could not set %@ (image: %d, status button: %d)", file, image != nil, button != nil);
            return;
        }
        image.template = YES;
        image.size = NSMakeSize(width, height);
        button.image = image;
    });
}
