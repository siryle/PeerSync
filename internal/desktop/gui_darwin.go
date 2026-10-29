//go:build darwin && cgo

package desktop

/*
#cgo darwin CFLAGS: -x objective-c -fobjc-arc
#cgo darwin LDFLAGS: -framework Cocoa -framework WebKit

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include <stdlib.h>

static NSWindow *mainWindow;
static id appDelegate;

@interface PeerSyncDelegate : NSObject <NSApplicationDelegate>
@end

@implementation PeerSyncDelegate
- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)sender {
	(void)sender;
	return YES;
}
@end

static NSMenuItem *menuItem(NSString *title, SEL action, NSString *key) {
	return [[NSMenuItem alloc] initWithTitle:title action:action keyEquivalent:key];
}

static void installMenu(void) {
	NSMenu *bar = [NSMenu new];
	NSMenuItem *appItem = [NSMenuItem new];
	[bar addItem:appItem];
	NSMenu *appMenu = [NSMenu new];
	[appMenu addItem:menuItem(@"退出 PeerSync", @selector(terminate:), @"q")];
	[appItem setSubmenu:appMenu];

	NSMenuItem *editItem = [NSMenuItem new];
	[bar addItem:editItem];
	NSMenu *edit = [[NSMenu alloc] initWithTitle:@"编辑"];
	[edit addItem:menuItem(@"剪切", @selector(cut:), @"x")];
	[edit addItem:menuItem(@"复制", @selector(copy:), @"c")];
	[edit addItem:menuItem(@"粘贴", @selector(paste:), @"v")];
	[edit addItem:menuItem(@"全选", @selector(selectAll:), @"a")];
	[editItem setSubmenu:edit];
	[NSApp setMainMenu:bar];
}

static void runApp(const char *title, const char *url, const void *icon, int iconLen) {
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
		appDelegate = [PeerSyncDelegate new];
		[NSApp setDelegate:appDelegate];
		installMenu();
		if (icon != NULL && iconLen > 0) {
			NSData *data = [NSData dataWithBytes:icon length:(NSUInteger)iconLen];
			NSImage *image = [[NSImage alloc] initWithData:data];
			if (image != nil) {
				[NSApp setApplicationIconImage:image];
			}
		}
		NSRect rect = NSMakeRect(0, 0, 1080, 720);
		mainWindow = [[NSWindow alloc] initWithContentRect:rect
			styleMask:(NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable)
			backing:NSBackingStoreBuffered
			defer:NO];
		mainWindow.title = [NSString stringWithUTF8String:title];
		mainWindow.minSize = NSMakeSize(880, 560);
		mainWindow.backgroundColor = [NSColor colorWithSRGBRed:0.953 green:0.933 blue:0.902 alpha:1.0];
		[mainWindow center];

		WKWebView *web = [[WKWebView alloc] initWithFrame:rect configuration:[WKWebViewConfiguration new]];
		web.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
		mainWindow.contentView = web;
		NSURL *nsurl = [NSURL URLWithString:[NSString stringWithUTF8String:url]];
		[web loadRequest:[NSURLRequest requestWithURL:nsurl]];
		[mainWindow makeKeyAndOrderFront:nil];
		[NSApp activateIgnoringOtherApps:YES];
		[NSApp run];
	}
}

// Called from a background thread. dispatch_sync onto the main queue, which is
// inside [NSApp run], so the panel is modal without stalling the page load.
static char *pickFolder(void) {
	__block char *out = NULL;
	dispatch_sync(dispatch_get_main_queue(), ^{
		NSOpenPanel *panel = [NSOpenPanel openPanel];
		panel.canChooseFiles = NO;
		panel.canChooseDirectories = YES;
		panel.canCreateDirectories = YES;
		panel.prompt = @"选择";
		panel.message = @"选择要同步的文件夹";
		if ([panel runModal] == NSModalResponseOK && panel.URL.path != nil) {
			out = strdup(panel.URL.path.UTF8String);
		}
	});
	return out;
}

static void showAlert(const char *msg) {
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
		NSAlert *alert = [NSAlert new];
		alert.messageText = @"PeerSync";
		alert.informativeText = [NSString stringWithUTF8String:msg ? msg : ""];
		[alert addButtonWithTitle:@"好"];
		[NSApp activateIgnoringOtherApps:YES];
		[alert runModal];
	}
}
*/
import "C"
import (
	"unsafe"

	"github.com/netcraft/peersync/internal/ui"
)

func Available() bool { return true }

func alert(msg string) {
	raw := C.CString(msg)
	defer C.free(unsafe.Pointer(raw))
	C.showAlert(raw)
}

func pickFolder() (string, error) {
	p := C.pickFolder()
	if p == nil {
		return "", ui.ErrCancelled
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoString(p), nil
}

func openWindow(page string, ctrl *ui.Controller) error {
	ctrl.SetPicker(pickFolder)
	title := C.CString("PeerSync")
	raw := C.CString(page)
	defer C.free(unsafe.Pointer(title))
	defer C.free(unsafe.Pointer(raw))
	logo, err := ui.LogoPNG()
	if err != nil {
		return err
	}
	C.runApp(title, raw, unsafe.Pointer(&logo[0]), C.int(len(logo)))
	return nil
}
