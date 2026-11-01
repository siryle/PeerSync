#import "../gui_darwin.m"

static NSString *testState;
static int lastAction;
static BOOL finishOnQuit;

char *peerSyncMenuState(void) { return strdup(testState.UTF8String); }
void peerSyncMenuAction(int action) {
	lastAction = action;
	if (finishOnQuit && action == PeerSyncQuit) psFinishQuit();
}

int main(int argc, char **argv) {
	@autoreleasepool {
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
		appDelegate = [PeerSyncDelegate new];
		[NSApp setDelegate:appDelegate];
		testState = @"{\"title\":\"同步运行中\",\"summary\":\"2 个文件夹 · 1 台设备已连接\",\"version\":\"0.3.0\",\"running\":true,\"folders\":[{\"label\":\"docs\",\"path\":\"/tmp/docs\"},{\"label\":\"work\",\"path\":\"/tmp/work\"}]}";
		installMenu();
		[appDelegate menuWillOpen:statusItem.menu];
		NSCAssert([statusTitle.title isEqualToString:@"同步运行中"], @"Running status missing");
		NSCAssert([toggleItem.title isEqualToString:@"暂停同步"] && toggleItem.enabled && reconnectItem.enabled, @"Running controls incorrect");
		NSCAssert(foldersItem.submenu.numberOfItems == 2, @"Folder submenu missing");
		NSCAssert([[foldersItem.submenu itemAtIndex:0].representedObject isEqual:@"/tmp/docs"], @"Folder path incorrect");
		NSCAssert(statusItem.button.image.isTemplate, @"Status icon must follow system appearance");
		for (NSMenu *menu in @[statusItem.menu, foldersItem.submenu]) {
			for (NSMenuItem *item in menu.itemArray) {
				NSCAssert(item.image == nil && item.indentationLevel == 0, @"Menu labels must align without row icons: %@", item.title);
			}
		}
		NSCAssert([quitItem.keyEquivalent isEqualToString:@""], @"Quit must not bind Command-Q");
		for (NSMenuItem *root in NSApp.mainMenu.itemArray) {
			for (NSMenuItem *item in root.submenu.itemArray) {
				NSCAssert(item.action != @selector(terminate:) && ![item.keyEquivalent isEqualToString:@"q"], @"Another quit entry exists");
			}
		}

		mainWindow = [[NSWindow alloc] initWithContentRect:NSMakeRect(0, 0, 400, 240)
			styleMask:(NSWindowStyleMaskTitled | NSWindowStyleMaskClosable) backing:NSBackingStoreBuffered defer:NO];
		mainWindow.releasedWhenClosed = NO;
		mainWindow.delegate = appDelegate;
		[appDelegate showWindow:nil];
		[mainWindow performClose:nil];
		NSCAssert(!mainWindow.isVisible && mainWindow.contentView != nil, @"Closing must hide and retain the window");
		NSCAssert(![appDelegate applicationShouldTerminateAfterLastWindowClosed:NSApp], @"Window close must not terminate");
		NSCAssert([appDelegate applicationShouldTerminate:NSApp] == NSTerminateCancel, @"Other termination requests must be rejected");
		[appDelegate applicationShouldHandleReopen:NSApp hasVisibleWindows:NO];
		NSCAssert(mainWindow.isVisible, @"Dock reopen must restore the window");
		[appDelegate hideWindow:nil];
		[appDelegate showWindow:nil];
		NSCAssert(mainWindow.isVisible, @"Menu must restore the window");
		[appDelegate toggleSync:nil];
		NSCAssert(lastAction == PeerSyncToggle, @"Pause action missing");
		[appDelegate reconnect:nil];
		NSCAssert(lastAction == PeerSyncReconnect, @"Reconnect action missing");
		[appDelegate quit:nil];
		NSCAssert(lastAction == PeerSyncQuit, @"Quit action missing");

		testState = @"{\"title\":\"同步已暂停\",\"summary\":\"0 个文件夹 · 0 台设备已连接\",\"running\":false,\"folders\":[]}";
		[appDelegate menuWillOpen:statusItem.menu];
		NSCAssert([toggleItem.title isEqualToString:@"恢复同步"] && toggleItem.enabled, @"Resume action missing");
		NSCAssert(!reconnectItem.enabled && !foldersItem.enabled, @"Paused/empty controls incorrect");
		testState = @"{\"title\":\"正在退出…\",\"quitting\":true}";
		[appDelegate refreshStatus:nil];
		NSCAssert(!toggleItem.enabled && !reconnectItem.enabled && !quitItem.enabled, @"Quitting controls must be disabled");

		if (argc > 1) {
			NSData *png = [[NSBitmapImageRep imageRepWithData:menuBarIcon().TIFFRepresentation]
				representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
			NSString *path = [[NSString stringWithUTF8String:argv[1]] stringByAppendingPathComponent:@"menubar-icon.png"];
			NSCAssert([png writeToFile:path atomically:YES], @"Could not render status icon");
		}
		[mainWindow orderOut:nil];
		[[NSStatusBar systemStatusBar] removeStatusItem:statusItem];

		finishOnQuit = YES;
		testState = @"{\"title\":\"同步运行中\",\"running\":true,\"folders\":[]}";
		dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 200 * NSEC_PER_MSEC), dispatch_get_main_queue(), ^{
			[mainWindow performClose:nil];
			NSCAssert(NSApp.isRunning && !mainWindow.isVisible, @"Hidden window must leave the app running");
			[appDelegate quit:nil];
		});
		psRunApp("PeerSync test", "about:blank", NULL, 0);
		NSCAssert(NSApp.delegate == nil && statusItem == nil, @"Menu quit must return and clean up the app loop");
	}
	return 0;
}
