//go:build darwin && cgo

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include <stdlib.h>
#include "gui_darwin.h"

static NSWindow *mainWindow;
static WKWebView *mainWeb;
static NSStatusItem *statusItem;
static NSMenuItem *statusTitle, *statusSummary, *toggleItem, *reconnectItem, *foldersItem, *quitItem;
static NSTimer *statusTimer;
static NSString *pendingPageAction;

@interface PeerSyncDelegate : NSObject <NSApplicationDelegate, NSWindowDelegate, NSMenuDelegate, WKNavigationDelegate>
- (void)showWindow:(id)sender;
- (void)hideWindow:(id)sender;
- (void)showSettings:(id)sender;
- (void)showLogs:(id)sender;
- (void)showAbout:(id)sender;
- (void)toggleSync:(id)sender;
- (void)reconnect:(id)sender;
- (void)quit:(id)sender;
- (void)openFolder:(NSMenuItem *)sender;
- (void)refreshStatus:(id)sender;
@end

static PeerSyncDelegate *appDelegate;

@interface PeerSyncTextMenuItem : NSMenuItem
@end

@implementation PeerSyncTextMenuItem
// AppKit can infer an action icon even when the stored image is nil.
- (NSImage *)image { return nil; }
@end

static NSMenuItem *menuItem(NSString *title, SEL action, NSString *key) {
	NSMenuItem *item = [[PeerSyncTextMenuItem alloc] initWithTitle:title action:action keyEquivalent:key];
	if (action != NULL) item.target = appDelegate;
	return item;
}

static NSDictionary *menuState(void) {
	char *raw = peerSyncMenuState();
	if (raw == NULL) return nil;
	NSData *data = [[NSString stringWithUTF8String:raw] dataUsingEncoding:NSUTF8StringEncoding];
	free(raw);
	id state = data ? [NSJSONSerialization JSONObjectWithData:data options:0 error:NULL] : nil;
	return [state isKindOfClass:[NSDictionary class]] ? state : nil;
}

static NSImage *menuBarIcon(void) {
	NSImage *image = [NSImage imageWithSize:NSMakeSize(20, 18) flipped:NO drawingHandler:^BOOL(NSRect rect) {
		(void)rect;
		[[NSColor blackColor] set];
		NSBezierPath *left = [NSBezierPath bezierPath];
		[left moveToPoint:NSMakePoint(3, 1)];
		[left curveToPoint:NSMakePoint(1, 4) controlPoint1:NSMakePoint(1, 1) controlPoint2:NSMakePoint(1, 2)];
		[left lineToPoint:NSMakePoint(1, 14)];
		[left curveToPoint:NSMakePoint(3, 17) controlPoint1:NSMakePoint(1, 16) controlPoint2:NSMakePoint(1, 17)];
		[left lineToPoint:NSMakePoint(6, 17)];
		[left lineToPoint:NSMakePoint(9, 14)];
		[left lineToPoint:NSMakePoint(9, 4)];
		[left curveToPoint:NSMakePoint(7, 1) controlPoint1:NSMakePoint(9, 2) controlPoint2:NSMakePoint(9, 1)];
		[left closePath];
		[left fill];
		[[NSBezierPath bezierPathWithRoundedRect:NSMakeRect(12, 1, 7, 13) xRadius:3.5 yRadius:3.5] fill];
		NSBezierPath *bridge = [NSBezierPath bezierPath];
		[bridge moveToPoint:NSMakePoint(6, 7)];
		[bridge curveToPoint:NSMakePoint(15, 7) controlPoint1:NSMakePoint(9, 11) controlPoint2:NSMakePoint(12, 11)];
		bridge.lineWidth = 2;
		bridge.lineCapStyle = NSLineCapStyleRound;
		[bridge stroke];
		return YES;
	}];
	image.template = YES;
	image.accessibilityDescription = @"PeerSync";
	return image;
}

static void installMenu(void) {
	NSMenu *bar = [NSMenu new];
	NSMenuItem *appItem = [NSMenuItem new];
	[bar addItem:appItem];
	NSMenu *appMenu = [NSMenu new];
	[appMenu addItem:menuItem(@"关于 PeerSync", @selector(showAbout:), @"")];
	[appMenu addItem:menuItem(@"设置…", @selector(showSettings:), @",")];
	[appItem setSubmenu:appMenu];

	NSMenuItem *editItem = [NSMenuItem new];
	[bar addItem:editItem];
	NSMenu *edit = [[NSMenu alloc] initWithTitle:@"编辑"];
	for (NSArray *entry in @[@[@"剪切", @"cut:", @"x"], @[@"复制", @"copy:", @"c"],
		@[@"粘贴", @"paste:", @"v"], @[@"全选", @"selectAll:", @"a"]]) {
		NSMenuItem *item = menuItem(entry[0], NSSelectorFromString(entry[1]), entry[2]);
		item.target = nil;
		[edit addItem:item];
	}
	[editItem setSubmenu:edit];
	NSMenuItem *windowItem = [NSMenuItem new];
	[bar addItem:windowItem];
	NSMenu *window = [[NSMenu alloc] initWithTitle:@"窗口"];
	[window addItem:menuItem(@"关闭窗口", @selector(hideWindow:), @"w")];
	[window addItem:menuItem(@"显示主窗口", @selector(showWindow:), @"")];
	[windowItem setSubmenu:window];
	[NSApp setMainMenu:bar];

	statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSSquareStatusItemLength];
	statusItem.button.image = menuBarIcon();
	statusItem.button.accessibilityLabel = @"PeerSync 菜单";
	NSMenu *menu = [NSMenu new];
	menu.autoenablesItems = NO;
	menu.delegate = appDelegate;
	NSMenuItem *heading = menuItem(@"PeerSync", NULL, @"");
	heading.enabled = NO;
	[menu addItem:heading];
	statusTitle = menuItem(@"正在读取状态…", NULL, @"");
	statusSummary = menuItem(@"", NULL, @"");
	statusTitle.enabled = statusSummary.enabled = NO;
	[menu addItem:statusTitle];
	[menu addItem:statusSummary];
	[menu addItem:[NSMenuItem separatorItem]];
	[menu addItem:menuItem(@"显示主窗口", @selector(showWindow:), @"")];
	toggleItem = menuItem(@"暂停同步", @selector(toggleSync:), @"");
	reconnectItem = menuItem(@"重新连接", @selector(reconnect:), @"");
	foldersItem = menuItem(@"打开同步文件夹", NULL, @"");
	[menu addItem:toggleItem];
	[menu addItem:reconnectItem];
	[menu addItem:foldersItem];
	[menu addItem:[NSMenuItem separatorItem]];
	[menu addItem:menuItem(@"查看同步日志", @selector(showLogs:), @"")];
	[menu addItem:menuItem(@"设置…", @selector(showSettings:), @"")];
	[menu addItem:menuItem(@"关于 PeerSync", @selector(showAbout:), @"")];
	[menu addItem:[NSMenuItem separatorItem]];
	quitItem = menuItem(@"退出 PeerSync", @selector(quit:), @"");
	[menu addItem:quitItem];
	statusItem.menu = menu;
}

@implementation PeerSyncDelegate
- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)sender {
	(void)sender;
	return NO;
}
- (NSApplicationTerminateReply)applicationShouldTerminate:(NSApplication *)sender {
	(void)sender;
	// Normal termination is initiated only by our status-menu action.
	return NSTerminateCancel;
}
- (BOOL)windowShouldClose:(NSWindow *)sender {
	[sender orderOut:nil];
	return NO;
}
- (BOOL)applicationShouldHandleReopen:(NSApplication *)sender hasVisibleWindows:(BOOL)visible {
	(void)sender;
	(void)visible;
	[self showWindow:nil];
	return NO;
}
- (void)showWindow:(id)sender {
	(void)sender;
	if (mainWindow.isMiniaturized) [mainWindow deminiaturize:nil];
	[mainWindow makeKeyAndOrderFront:nil];
	[NSApp activateIgnoringOtherApps:YES];
}
- (void)hideWindow:(id)sender {
	(void)sender;
	[mainWindow orderOut:nil];
}
- (void)openPageAction:(NSString *)action {
	[self showWindow:nil];
	pendingPageAction = action;
	if (!mainWeb.isLoading) [self deliverPageAction];
}
- (void)deliverPageAction {
	if (pendingPageAction == nil) return;
	NSString *action = pendingPageAction;
	NSString *script = [NSString stringWithFormat:
		@"typeof window.peerSyncDesktopAction === 'function' && (window.peerSyncDesktopAction('%@'), true)", action];
	[mainWeb evaluateJavaScript:script completionHandler:^(id result, NSError *error) {
		if (!error && [result boolValue] && [pendingPageAction isEqualToString:action]) pendingPageAction = nil;
	}];
}
- (void)webView:(WKWebView *)webView didFinishNavigation:(WKNavigation *)navigation {
	(void)webView;
	(void)navigation;
	[self deliverPageAction];
}
- (void)showSettings:(id)sender {
	(void)sender;
	[self openPageAction:@"settings"];
}
- (void)showLogs:(id)sender {
	(void)sender;
	[self openPageAction:@"logs"];
}
- (void)showAbout:(id)sender {
	(void)sender;
	NSString *version = menuState()[@"version"] ?: @"";
	[NSApp orderFrontStandardAboutPanelWithOptions:@{
		NSAboutPanelOptionApplicationName: @"PeerSync",
		NSAboutPanelOptionApplicationVersion: version,
		NSAboutPanelOptionVersion: @""
	}];
	[NSApp activateIgnoringOtherApps:YES];
}
- (void)toggleSync:(id)sender {
	(void)sender;
	peerSyncMenuAction(PeerSyncToggle);
	[self refreshStatus:nil];
}
- (void)reconnect:(id)sender {
	(void)sender;
	peerSyncMenuAction(PeerSyncReconnect);
	[self refreshStatus:nil];
}
- (void)quit:(id)sender {
	(void)sender;
	peerSyncMenuAction(PeerSyncQuit);
	[self refreshStatus:nil];
}
- (void)openFolder:(NSMenuItem *)sender {
	NSURL *url = [NSURL fileURLWithPath:sender.representedObject isDirectory:YES];
	if (![[NSWorkspace sharedWorkspace] openURL:url]) psShowAlert("无法打开同步文件夹，请检查目录是否仍然存在。");
}
- (void)refreshStatus:(id)sender {
	(void)sender;
	NSDictionary *state = menuState();
	BOOL running = [state[@"running"] boolValue];
	BOOL available = state != nil && ![state[@"busy"] boolValue] && ![state[@"quitting"] boolValue];
	statusTitle.title = state[@"title"] ?: @"读取状态失败";
	statusTitle.toolTip = state[@"error"];
	statusSummary.title = state[@"summary"] ?: @"";
	toggleItem.title = running ? @"暂停同步" : @"恢复同步";
	toggleItem.enabled = available;
	reconnectItem.enabled = available && running;
	quitItem.enabled = state != nil && ![state[@"quitting"] boolValue];
	statusItem.button.alphaValue = running || [state[@"error"] length] ? 1.0 : 0.55;
	statusItem.button.toolTip = [NSString stringWithFormat:@"PeerSync — %@%@", statusTitle.title,
		[state[@"error"] length] ? [@"\n" stringByAppendingString:state[@"error"]] : @""];
}
- (void)menuWillOpen:(NSMenu *)menu {
	(void)menu;
	[self refreshStatus:nil];
	NSMenu *folders = [NSMenu new];
	folders.autoenablesItems = NO;
	NSArray *list = menuState()[@"folders"] ?: @[];
	NSCountedSet *labels = [NSCountedSet new];
	for (NSDictionary *folder in list) [labels addObject:folder[@"label"]];
	for (NSDictionary *folder in list) {
		NSString *label = [folder[@"label"] length] ? folder[@"label"] : [folder[@"path"] lastPathComponent];
		if ([labels countForObject:folder[@"label"]] > 1) label = [NSString stringWithFormat:@"%@ — %@", label, folder[@"path"]];
		NSMenuItem *item = menuItem(label, @selector(openFolder:), @"");
		item.representedObject = folder[@"path"];
		item.toolTip = folder[@"path"];
		[folders addItem:item];
	}
	foldersItem.submenu = folders;
	foldersItem.enabled = list.count > 0;
}
@end

void psRunApp(const char *title, const char *url, const void *icon, int iconLen) {
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
		appDelegate = [PeerSyncDelegate new];
		[NSApp setDelegate:appDelegate];
		installMenu();
		if (icon != NULL && iconLen > 0) {
			NSData *data = [NSData dataWithBytes:icon length:(NSUInteger)iconLen];
			NSImage *image = [[NSImage alloc] initWithData:data];
			if (image != nil) [NSApp setApplicationIconImage:image];
		}
		NSRect rect = NSMakeRect(0, 0, 1080, 720);
		mainWindow = [[NSWindow alloc] initWithContentRect:rect
			styleMask:(NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable)
			backing:NSBackingStoreBuffered defer:NO];
		mainWindow.releasedWhenClosed = NO;
		mainWindow.delegate = appDelegate;
		mainWindow.title = [NSString stringWithUTF8String:title];
		mainWindow.minSize = NSMakeSize(880, 560);
		mainWindow.backgroundColor = [NSColor colorWithSRGBRed:0.953 green:0.933 blue:0.902 alpha:1.0];
		[mainWindow center];
		mainWeb = [[WKWebView alloc] initWithFrame:rect configuration:[WKWebViewConfiguration new]];
		mainWeb.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
		mainWeb.navigationDelegate = appDelegate;
		mainWindow.contentView = mainWeb;
		[mainWeb loadRequest:[NSURLRequest requestWithURL:[NSURL URLWithString:[NSString stringWithUTF8String:url]]]];
		statusTimer = [NSTimer timerWithTimeInterval:2 target:appDelegate selector:@selector(refreshStatus:) userInfo:nil repeats:YES];
		[[NSRunLoop mainRunLoop] addTimer:statusTimer forMode:NSRunLoopCommonModes];
		[appDelegate refreshStatus:nil];
		[appDelegate showWindow:nil];
		[NSApp run];
		[statusTimer invalidate];
		[[NSStatusBar systemStatusBar] removeStatusItem:statusItem];
		[mainWindow orderOut:nil];
		mainWeb.navigationDelegate = nil;
		mainWindow.delegate = nil;
		[NSApp setDelegate:nil];
		mainWeb = nil;
		mainWindow = nil;
		statusItem = nil;
		appDelegate = nil;
	}
}

void psFinishQuit(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		if (NSApp.modalWindow != nil) {
			[NSApp.modalWindow orderOut:nil];
			[NSApp stopModalWithCode:NSModalResponseCancel];
		}
		[NSApp stop:nil];
		NSEvent *wake = [NSEvent otherEventWithType:NSEventTypeApplicationDefined location:NSZeroPoint
			modifierFlags:0 timestamp:0 windowNumber:0 context:nil subtype:0 data1:0 data2:0];
		[NSApp postEvent:wake atStart:YES];
	});
}

char *psPickFolder(void) {
	__block char *out = NULL;
	dispatch_sync(dispatch_get_main_queue(), ^{
		NSOpenPanel *panel = [NSOpenPanel openPanel];
		panel.canChooseFiles = NO;
		panel.canChooseDirectories = YES;
		panel.canCreateDirectories = YES;
		panel.prompt = @"选择";
		panel.message = @"选择要同步的文件夹";
		if ([panel runModal] == NSModalResponseOK && panel.URL.path != nil) out = strdup(panel.URL.path.UTF8String);
	});
	return out;
}

void psShowAlert(const char *msg) {
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
