const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const events = [];
const elements = new Map();
const document = {
  getElementById(id) {
    if (!elements.has(id)) elements.set(id, {
      innerHTML: "", hidden: false,
      scrollIntoView() { events.push("scroll:" + id); },
      focus() { events.push("focus:" + id); },
    });
    return elements.get(id);
  },
  addEventListener() {},
};
const context = vm.createContext({
  document, window: {}, events,
  location: { hash: "" },
  sessionStorage: { getItem() { return ""; } },
  fetch: () => new Promise(() => {}),
  setInterval() {},
});
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), context);
vm.runInContext(`
  render = () => events.push("render");
  openDevice = () => { events.push("settings"); modalOpen = true; };
`, context);

context.window.peerSyncDesktopAction("settings");
assert.deepEqual(events, []);
vm.runInContext("state = { shares: [] }; applyDesktopAction();", context);
assert.deepEqual(events, ["settings"]);

context.window.peerSyncDesktopAction("logs");
assert.deepEqual(events, ["settings"]);
vm.runInContext("closeModal();", context);
assert.deepEqual(events, ["settings", "render", "render", "scroll:log", "focus:log"]);
assert.equal(document.getElementById("log").tabIndex, -1);

context.window.peerSyncDesktopAction("quit");
assert.equal(events.length, 5);
context.window.peerSyncDesktopAction("settings");
assert.equal(events.at(-1), "settings");
