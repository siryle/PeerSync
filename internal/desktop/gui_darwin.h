#ifndef PEERSYNC_GUI_DARWIN_H
#define PEERSYNC_GUI_DARWIN_H

enum {
	PeerSyncToggle = 1,
	PeerSyncReconnect = 2,
	PeerSyncQuit = 3
};

extern char *peerSyncMenuState(void);
extern void peerSyncMenuAction(int action);

void psRunApp(const char *title, const char *url, const void *icon, int iconLen);
void psFinishQuit(void);
char *psPickFolder(void);
void psShowAlert(const char *msg);

#endif
