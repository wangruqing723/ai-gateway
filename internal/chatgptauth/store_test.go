package chatgptauth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// 双实例测试：同一进程内两个 Manager 打开同一路径。flock 按打开的文件描述锁定，
// 两个 Manager 各自打开锁文件，因此和两个进程一样互斥。

// openPeer 打开指向同一账户文件的第二个实例，并让两个实例每次都检查共享文件。
func (env *testEnv) openPeer() *Manager {
	env.t.Helper()
	env.m.syncIntervalOverride = -1
	peer := env.open()
	peer.syncIntervalOverride = -1
	return peer
}

func TestStoreRotatedRefreshIsNotExchangedAgain(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "old-refresh", env.clock.Now().Add(-time.Minute))
	peer := env.openPeer()
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "old-refresh" {
			http.Error(w, "reused", http.StatusBadRequest)
			return
		}
		writeJSONBody(w, http.StatusOK, tokenGrant("rotated-access", "rotated-refresh", "", 3600))
	})
	if token, _, err := env.m.AccessToken(context.Background(), id, nil); err != nil || token != "rotated-access" {
		t.Fatalf("第一个实例刷新: %q %v", token, err)
	}
	// 第二个实例内存里仍是旧凭据；取令牌时先合并磁盘，直接使用新令牌。
	if token, _, err := peer.AccessToken(context.Background(), id, nil); err != nil || token != "rotated-access" {
		t.Fatalf("第二个实例应直接使用已轮换的令牌: %q %v", token, err)
	}
	if calls := env.up.tokenCalls.Load(); calls != 1 {
		t.Fatalf("令牌交换次数 = %d，want 1", calls)
	}
}

func TestStoreConcurrentRefreshExchangesOnce(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "old-refresh", env.clock.Now().Add(-time.Minute))
	peer := env.openPeer()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		writeJSONBody(w, http.StatusOK, tokenGrant("rotated-access", "rotated-refresh", "", 3600))
	})
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for _, m := range []*Manager{env.m, peer} {
		go func(m *Manager) {
			token, _, err := m.AccessToken(context.Background(), id, nil)
			results <- token
			errs <- err
		}(m)
	}
	<-entered
	// 给另一个实例时间去争抢账户刷新锁：它必须阻塞在锁上，而不是用同一个旧 refresh token 再交换一次。
	time.Sleep(50 * time.Millisecond)
	// 交换期间全局存储锁空闲，另一实例的普通写入不受影响。
	note := "交换期间写入"
	if _, err := peer.UpdateMetadata(id, AccountMetadataPatch{Note: &note}); err != nil {
		t.Fatalf("交换期间普通写入不应等待: %v", err)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if token, err := <-results, <-errs; err != nil || token != "rotated-access" {
			t.Fatalf("并发刷新结果: %q %v", token, err)
		}
	}
	if calls := env.up.tokenCalls.Load(); calls != 1 {
		t.Fatalf("令牌交换次数 = %d，want 1", calls)
	}
}

func TestStoreInterleavedWritesMerge(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	peer := env.openPeer()
	note, tags := "来自实例一", []string{"peer"}
	if _, err := env.m.UpdateMetadata(id, AccountMetadataPatch{Note: &note}); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.UpdateMetadata(id, AccountMetadataPatch{Tags: &tags}); err != nil {
		t.Fatal(err)
	}
	second := env.addAccountTo(peer, "remote-2")
	view, ok := env.open().Account(id)
	if !ok || view.Note != note || len(view.Tags) != 1 || view.Tags[0] != "peer" {
		t.Fatalf("两个实例的写入应合并: %#v", view)
	}
	if _, ok := env.m.Account(second); !ok {
		t.Fatal("另一实例新增的账户应出现在本实例")
	}
	if err := peer.Delete(second); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.m.Account(second); ok {
		t.Fatal("另一实例删除的账户应从本实例移除")
	}
}

// addAccountTo 经指定实例的写入链路新增一个假账户。
func (env *testEnv) addAccountTo(m *Manager, remoteID string) string {
	env.t.Helper()
	unlock, err := m.lockForWrite(context.Background())
	if err != nil {
		env.t.Fatal(err)
	}
	defer unlock()
	id, err := randomHex(16)
	if err != nil {
		env.t.Fatal(err)
	}
	item := &account{ID: "acc_" + id, AccountID: remoteID, AccessToken: "fake-access-" + remoteID,
		RefreshToken: "fake-refresh-" + remoteID, ExpiresAt: env.clock.Now().Add(time.Hour)}
	m.accounts[item.ID] = item
	if err := m.persistLocked(); err != nil {
		env.t.Fatal(err)
	}
	return item.ID
}

func TestStorePausePropagatesAndWakesWaiters(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	peer := env.openPeer()
	watch, err := env.m.WatchAdmission(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Release()
	if _, changed, _, err := peer.SetPaused(id, true, peer.Version()); err != nil || !changed {
		t.Fatalf("另一实例暂停: %v %v", changed, err)
	}
	// 本进程没有任何新请求，排队中的等待者也要靠共享文件轮询在约 1 秒内被唤醒。
	select {
	case <-watch.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("另一实例的暂停没有唤醒本进程的等待者")
	}
	if !watch.Paused() || !env.m.IsPaused(id) {
		t.Fatal("等待者应以暂停原因结束，本实例也应看到暂停")
	}
	if _, err := env.m.WatchAdmission(context.Background(), id); !errors.Is(err, ErrAccountPaused) {
		t.Fatalf("暂停后新的准入应被拒绝: %v", err)
	}
}

func TestStoreMergeKeepsUnsavedCredentials(t *testing.T) {
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "old-refresh", env.clock.Now().Add(-time.Minute))
	peer := env.openPeer()
	env.up.setToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, tokenGrant("rotated-access", "rotated-refresh", "", 3600))
	})
	restore := env.failPersist()
	if _, _, err := env.m.AccessToken(context.Background(), id, nil); err == nil {
		t.Fatal("落盘失败时取令牌应报错")
	}
	restore()
	// 另一实例用磁盘上的旧凭据写入元数据；本实例合并时不能用旧值覆盖内存里的新凭据。
	note := "peer"
	if _, err := peer.UpdateMetadata(id, AccountMetadataPatch{Note: &note}); err != nil {
		t.Fatal(err)
	}
	if token, _, err := env.m.AccessToken(context.Background(), id, nil); err != nil || token != "rotated-access" {
		t.Fatalf("合并后应保留未保存的新凭据: %q %v", token, err)
	}
	if view, _ := env.m.Account(id); view.Note != note {
		t.Fatalf("合并后应看到另一实例的备注: %#v", view)
	}
	if calls := env.up.tokenCalls.Load(); calls != 1 {
		t.Fatalf("令牌交换次数 = %d，want 1", calls)
	}
}

func TestStoreBusyWriteTimesOutWithoutWriting(t *testing.T) {
	if !crossProcessLocking {
		t.Skip("当前平台没有跨进程文件锁")
	}
	env := newTestEnv(t)
	id := env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	peer := env.openPeer()
	peer.storeWaitOverride = 100 * time.Millisecond
	before, err := os.ReadFile(env.path)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := env.m.lockForWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	note := "blocked"
	_, err = peer.UpdateMetadata(id, AccountMetadataPatch{Note: &note})
	unlock()
	if !errors.Is(err, ErrAccountStoreBusy) || !errors.Is(err, ErrAccountMetadataPersist) {
		t.Fatalf("等不到存储锁应返回存储忙且沿用原有保存失败错误: %v", err)
	}
	after, err := os.ReadFile(env.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("存储忙时不应写入任何内容")
	}
	if view, _ := peer.Account(id); view.Note != "" {
		t.Fatalf("存储忙时内存也不应改变: %#v", view)
	}
}

// TestStoreLockHelperProcess 只在子进程中运行：取得存储锁后不释放就退出，验证内核会释放 flock。
func TestStoreLockHelperProcess(t *testing.T) {
	path := os.Getenv("CHATGPTAUTH_LOCK_HELPER_PATH")
	if path == "" {
		t.Skip("仅作为子进程运行")
	}
	m := &Manager{path: path, accounts: map[string]*account{}}
	if err := m.acquireStore(context.Background()); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestStoreLockReleasedWhenProcessExits(t *testing.T) {
	if !crossProcessLocking {
		t.Skip("当前平台没有跨进程文件锁")
	}
	env := newTestEnv(t)
	env.addAccount("remote-1", "one@example.invalid", "fake-refresh", env.clock.Now().Add(time.Hour))
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreLockHelperProcess$")
	cmd.Env = append(os.Environ(), "CHATGPTAUTH_LOCK_HELPER_PATH="+env.path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("子进程取锁失败: %v %s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	unlock, err := env.m.lockForWrite(ctx)
	if err != nil {
		t.Fatalf("子进程退出后应能取得存储锁: %v", err)
	}
	unlock()
}
