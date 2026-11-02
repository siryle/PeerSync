package syncer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/netcraft/peersync/internal/vclock"
)

func TestIndexStreamingRoundTripAndLegacyCompatibility(t *testing.T) {
	e := testEngine(t, "self")
	idx := indexFile{Seq: 17, Files: map[string]*File{
		"nested/quote\"\t<&>.txt": {Size: 1, Sum: [32]byte{1, 255}, Blocks: [][32]byte{{2, 254}}, ModTime: 1234, Mode: 0o640, Version: vclock.Vector{"self": 7, "peer": 2}},
		"deleted.txt":             {Deleted: true, Version: vclock.Vector{"self": 7, "peer": 2}},
		"directory":               {IsDir: true, Mode: 0o750, Version: vclock.Vector{"self": 1}, Local: true},
	}}
	legacy, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	var streamed bytes.Buffer
	if err := writeIndex(&streamed, idx); err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{legacy, streamed.Bytes()} {
		if err := os.WriteFile(e.IndexPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := e.load(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(e.idx, idx) {
			t.Fatalf("index changed during load: %#v", e.idx)
		}
		if fmt.Sprintf("%p", e.idx.Files["deleted.txt"].Version) != fmt.Sprintf("%p", e.idx.Files["nested/quote\"\t<&>.txt"].Version) {
			t.Fatal("identical loaded clocks do not share storage")
		}
	}
	var decoded indexFile
	if err := json.Unmarshal(streamed.Bytes(), &decoded); err != nil || !reflect.DeepEqual(decoded, idx) {
		t.Fatalf("streamed format cannot be read by the old decoder: %v", err)
	}
}

func TestIndexLoadDoesNotPublishPartialOrCorruptData(t *testing.T) {
	e := testEngine(t, "self")
	for _, body := range []string{
		`{"seq":2,"files":{"a":{"version":{"self":1}}}`, // truncated outer object
		`{"files":{"a":null}}`,
		`{"seq":-1,"files":{}}`,
		`{"files":[]}`,
		`{"files":{}} {}`,
		`{"files":{}} garbage`,
	} {
		e.idx = indexFile{Seq: 99, Files: map[string]*File{"keep": {Version: vclock.Vector{"self": 1}}}}
		if err := os.WriteFile(e.IndexPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := e.load(); err == nil {
			t.Fatalf("accepted corrupt index: %s", body)
		}
		if e.idx.Seq != 99 || len(e.idx.Files) != 1 || e.idx.Files["keep"] == nil {
			t.Fatal("failed load published a partial index")
		}
	}
	for _, body := range []string{`{}`, `{"files":null}`, `{"future":{"nested":[1,2]},"files":{},"seq":3}`} {
		if err := os.WriteFile(e.IndexPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := e.load(); err != nil || e.idx.Files == nil {
			t.Fatalf("valid empty index rejected: %s, %v", body, err)
		}
	}
}

type indexErrorWriter struct{ err error }

func (w indexErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestIndexWriteErrorsPreserveSavedIndex(t *testing.T) {
	want := errors.New("disk full")
	for _, name := range []string{"small", strings.Repeat("large", 20000)} {
		if err := writeIndex(indexErrorWriter{want}, indexFile{Files: map[string]*File{name: {}}}); !errors.Is(err, want) {
			t.Fatalf("writer failure not propagated: %v", err)
		}
	}
	e := testEngine(t, "self")
	if err := e.saveLocked(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(e.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.IndexPath+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	e.idx.Seq++
	if err := e.saveLocked(); err == nil {
		t.Fatal("expected temporary-file write failure")
	}
	after, err := os.ReadFile(e.IndexPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("write failure replaced the saved index: %v", err)
	}
}

func TestSharedVersionsSurviveEditsAndAnnouncementSnapshots(t *testing.T) {
	e := testEngine(t, "self")
	putFile(t, e.Root+"/a.txt", "a")
	putFile(t, e.Root+"/b.txt", "b")
	scan(t, e)
	if fmt.Sprintf("%p", e.idx.Files["a.txt"].Version) != fmt.Sprintf("%p", e.idx.Files["b.txt"].Version) {
		t.Fatal("new files retained duplicate version maps")
	}
	_, public := e.Entries()
	_, announcement := e.entries(false)
	for _, entry := range public {
		entry.Version["self"] = 99
	}
	putFile(t, e.Root+"/a.txt", "edited")
	scan(t, e)
	if versionOf(t, e, "a.txt")["self"] != 2 || versionOf(t, e, "b.txt")["self"] != 1 {
		t.Fatal("editing one file or public snapshot mutated another file's version")
	}
	for _, entry := range announcement {
		if entry.Version["self"] != 1 {
			t.Fatal("editing a file mutated an in-flight announcement")
		}
	}
}
