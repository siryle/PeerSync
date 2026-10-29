GO ?= go
BIN := bin
VERSION := $(shell awk -F'"' '/^const Current /{print $$2; exit}' internal/version/version.go)
BUILDN := $(shell awk -F'"' '/^const Build /{print $$2; exit}' internal/version/version.go)
WIN_LDFLAGS := -H windowsgui -X github.com/netcraft/peersync/internal/version.Window=1

.PHONY: test server client app windows-gui build-all dmg-universal clean

test:
	$(GO) test ./...

server:
	CGO_ENABLED=0 $(GO) build -o $(BIN)/peersync-server ./cmd/peersync-server

client:
	CGO_ENABLED=0 $(GO) build -o $(BIN)/peersync ./cmd/peersync

# macOS window. Requires CGO and Xcode command line tools.
app:
	mkdir -p $(BIN)/PeerSync.app/Contents/MacOS $(BIN)/PeerSync.app/Contents/Resources
	CGO_ENABLED=1 $(GO) build -o $(BIN)/PeerSync.app/Contents/MacOS/PeerSync ./cmd/peersync
	cp assets/Info.plist $(BIN)/PeerSync.app/Contents/Info.plist
	/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $(VERSION)" $(BIN)/PeerSync.app/Contents/Info.plist
	/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $(BUILDN)" $(BIN)/PeerSync.app/Contents/Info.plist
	cp assets/AppIcon.icns $(BIN)/PeerSync.app/Contents/Resources/AppIcon.icns

# Windows window. WebView2 is pure Go, so this cross-compiles from macOS or Linux.
windows-gui:
	mkdir -p $(BIN)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -ldflags "$(WIN_LDFLAGS)" -o $(BIN)/PeerSync-windows-amd64.exe ./cmd/peersync
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build -ldflags "$(WIN_LDFLAGS)" -o $(BIN)/PeerSync-windows-arm64.exe ./cmd/peersync

build-all: windows-gui
	mkdir -p $(BIN)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN)/peersync-linux-amd64 ./cmd/peersync
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -o $(BIN)/peersync-linux-arm64 ./cmd/peersync
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -o $(BIN)/peersync-darwin-amd64 ./cmd/peersync
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -o $(BIN)/peersync-darwin-arm64 ./cmd/peersync
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -o $(BIN)/peersync-windows-amd64.exe ./cmd/peersync
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build -o $(BIN)/peersync-windows-arm64.exe ./cmd/peersync
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o $(BIN)/peersync-server-linux-amd64 ./cmd/peersync-server
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -o $(BIN)/peersync-server-linux-arm64 ./cmd/peersync-server

# Universal macOS disk image: arm64 and Intel in one PeerSync.app.
# Used by the GitHub release workflow. Requires Xcode.
dmg-universal:
	mkdir -p build/arch
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 $(GO) build -o build/arch/PeerSync-arm64 ./cmd/peersync
	SDK="$$(xcrun --sdk macosx --show-sdk-path)"; \
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 \
		CC="clang -arch x86_64" \
		CGO_CFLAGS="-arch x86_64 -isysroot $$SDK" \
		CGO_LDFLAGS="-arch x86_64 -isysroot $$SDK" \
		$(GO) build -o build/arch/PeerSync-amd64 ./cmd/peersync
	rm -rf build/PeerSync.app build/dmg-stage
	mkdir -p build/PeerSync.app/Contents/MacOS build/PeerSync.app/Contents/Resources build/dmg-stage
	lipo -create build/arch/PeerSync-arm64 build/arch/PeerSync-amd64 -output build/PeerSync.app/Contents/MacOS/PeerSync
	cp assets/Info.plist build/PeerSync.app/Contents/Info.plist
	/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $(VERSION)" build/PeerSync.app/Contents/Info.plist
	/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $(BUILDN)" build/PeerSync.app/Contents/Info.plist
	cp assets/AppIcon.icns build/PeerSync.app/Contents/Resources/AppIcon.icns
	codesign --force --sign - build/PeerSync.app/Contents/MacOS/PeerSync
	codesign --force --sign - build/PeerSync.app
	cp -R build/PeerSync.app build/dmg-stage/PeerSync.app
	ln -s /Applications build/dmg-stage/Applications
	rm -f build/PeerSync-$(VERSION)-macos.dmg
	hdiutil create -volname PeerSync -srcfolder build/dmg-stage -ov -format UDZO -fs HFS+ build/PeerSync-$(VERSION)-macos.dmg
	rm -rf build/dmg-stage
	lipo -info build/PeerSync.app/Contents/MacOS/PeerSync

clean:
	rm -rf $(BIN) build
