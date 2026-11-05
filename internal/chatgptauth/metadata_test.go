package chatgptauth

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUpdateMetadataNormalizesTags(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	tags := []string{" 工作 ", "", "Team", "team", "工作", strings.Repeat("签", maxAccountTagLength)}
	view, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Tags: &tags})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"工作", "Team", strings.Repeat("签", maxAccountTagLength)}
	if !reflect.DeepEqual(view.Tags, want) || view.Note != "" {
		t.Fatalf("标签规范化 = %#v note=%q", view.Tags, view.Note)
	}
}

func TestUpdateMetadataRejectsInvalidInput(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	tooMany := make([]string, maxAccountTags+1)
	for i := range tooMany {
		tooMany[i] = "t" + strings.Repeat("x", i)
	}
	tooLong := []string{strings.Repeat("长", maxAccountTagLength+1)}
	longNote := strings.Repeat("注", maxAccountNoteLength+1)
	cases := []struct {
		name  string
		patch AccountMetadataPatch
	}{
		{"空补丁", AccountMetadataPatch{}},
		{"标签过多", AccountMetadataPatch{Tags: &tooMany}},
		{"标签过长", AccountMetadataPatch{Tags: &tooLong}},
		{"备注过长", AccountMetadataPatch{Note: &longNote}},
	}
	for _, tc := range cases {
		if _, err := env.m.UpdateMetadata(id, tc.patch); !errors.Is(err, ErrInvalidAccountMetadata) {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
	exact := tooMany[:maxAccountTags]
	maxNote := strings.Repeat("注", maxAccountNoteLength)
	view, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Tags: &exact, Note: &maxNote})
	if err != nil || len(view.Tags) != maxAccountTags || view.Note != maxNote {
		t.Fatalf("边界值应被接受: %v", err)
	}
	if _, err := env.m.UpdateMetadata("acc_missing", AccountMetadataPatch{Note: &maxNote}); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("不存在的账户 = %v", err)
	}
}

func TestUpdateMetadataPartialPatchAndRestart(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	tags := []string{"工作"}
	note := "第一版备注"
	if _, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Tags: &tags, Note: &note}); err != nil {
		t.Fatal(err)
	}
	newNote := "只改备注"
	view, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Note: &newNote})
	if err != nil || !reflect.DeepEqual(view.Tags, tags) || view.Note != newNote {
		t.Fatalf("只改备注不应影响标签: %#v %v", view, err)
	}
	empty := []string{}
	view, err = env.m.UpdateMetadata(id, AccountMetadataPatch{Tags: &empty})
	if err != nil || len(view.Tags) != 0 || view.Note != newNote {
		t.Fatalf("清空标签不应影响备注: %#v %v", view, err)
	}
	reopened, ok := env.open().Account(id)
	if !ok || len(reopened.Tags) != 0 || reopened.Note != newNote {
		t.Fatalf("重启后元数据 = %#v", reopened)
	}
}

func TestUpdateMetadataPersistFailureRollsBack(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	tags := []string{"原标签"}
	note := "原备注"
	if _, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Tags: &tags, Note: &note}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(env.path)
	if err != nil {
		t.Fatal(err)
	}
	restore := env.failPersist()
	newTags := []string{"新标签"}
	newNote := "新备注"
	if _, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Tags: &newTags, Note: &newNote}); !errors.Is(err, ErrAccountMetadataPersist) {
		t.Fatalf("落盘失败应返回 ErrAccountMetadataPersist，实际 %v", err)
	}
	restore()
	view, _ := env.m.Account(id)
	after, _ := os.ReadFile(env.path)
	if !reflect.DeepEqual(view.Tags, tags) || view.Note != note || string(after) != string(before) {
		t.Fatalf("落盘失败后内存或文件被改动: %#v", view)
	}
	entries, _ := os.ReadDir(filepath.Dir(env.path))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("落盘失败遗留临时文件 %s", entry.Name())
		}
	}
}

func TestExportExcludesLocalMetadata(t *testing.T) {
	env := newTestEnv(t)
	first := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	second := env.addAccount("remote-2", "two@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	tags := []string{"fake-tag-marker"}
	note := "fake-note-marker"
	for _, id := range []string{first, second} {
		if _, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Tags: &tags, Note: &note}); err != nil {
			t.Fatal(err)
		}
	}
	for _, format := range []string{"cockpit_tools", "cliproxyapi", "sub2api", "auth_json"} {
		for _, ids := range [][]string{{first}, {first, second}} {
			file, err := env.m.Export(format, ids)
			if err != nil {
				t.Fatalf("%s 导出 %d 个账户: %v", format, len(ids), err)
			}
			content := string(file.Data)
			if file.ContentType == "application/zip" {
				content = strings.Join(zipJSONEntries(t, file.Data), "\n")
			}
			if strings.Contains(content, "fake-tag-marker") || strings.Contains(content, "fake-note-marker") {
				t.Fatalf("%s 导出包含本地标签或备注", format)
			}
		}
	}
}
