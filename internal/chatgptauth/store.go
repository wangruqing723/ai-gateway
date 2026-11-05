package chatgptauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

// 跨进程协调（DESIGN 7.1，D06）。开发与正式实例共享同一份账户文件：
// 每个写入入口先取进程内存储锁，再取同目录下稳定存在的锁文件上的 flock，
// 读取并合并磁盘内容后再修改、原子替换。只给 rename 加锁不够：两个进程可以先读到同一份旧凭据。
//
// 令牌交换另用账户级刷新锁（<账户文件>.refresh/ 下按账户 ID 摘要命名的锁文件）在实例间串行，
// 交换期间不持有全局存储锁，只在交换前重新读取、交换后写回时短暂持有。
//
// 锁顺序：refreshMu / usageMu → 账户刷新锁 → storeMu（含文件锁）→ m.mu → invocationMu。
// statsMu、bgMu 是叶子锁。

// ErrAccountStoreBusy 表示等不到跨进程存储锁（另一个实例正在写入或刷新），本次没有写入任何内容。
var ErrAccountStoreBusy = errors.New("ChatGPT OAuth 账户存储正被另一个实例占用，请稍后重试")

const (
	// defaultStoreWait 是没有调用方 context 的写入入口等待存储锁的上限。
	defaultStoreWait = 30 * time.Second
	// sharedSyncInterval 是只读路径检查共享文件是否被其他实例改写的最小间隔。
	sharedSyncInterval = time.Second
)

// storeStamp 是账户文件的身份快照，用于只读路径快速判断文件是否被替换。
type storeStamp struct {
	info os.FileInfo
}

// lockFilePath 返回稳定的锁文件路径。它从不被替换，因此不同进程总是锁同一个 inode。
func (m *Manager) lockFilePath() string { return m.path + ".lock" }

// acquireStore 取得进程内存储锁与跨进程文件锁，ctx 结束前一直重试。成功后必须调用 releaseStore。
func (m *Manager) acquireStore(ctx context.Context) error {
	if err := m.storeMu.Lock(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrAccountStoreBusy, err)
	}
	file, err := m.openLockFile()
	if err != nil {
		m.storeMu.Unlock()
		return err
	}
	if err := waitLockFile(ctx, file); err != nil {
		_ = file.Close()
		m.storeMu.Unlock()
		return err
	}
	m.lockFile = file
	m.storeHeld.Store(true)
	return nil
}

// tryAcquireStore 只在两把锁都能立即取得时成功，供只读路径的机会性同步使用。
func (m *Manager) tryAcquireStore() bool {
	if !m.storeMu.TryLock() {
		return false
	}
	file, err := m.openLockFile()
	if err != nil {
		m.storeMu.Unlock()
		return false
	}
	locked, err := tryLockFile(file)
	if err != nil || !locked {
		_ = file.Close()
		m.storeMu.Unlock()
		return false
	}
	m.lockFile = file
	m.storeHeld.Store(true)
	return true
}

func (m *Manager) openLockFile() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return nil, fmt.Errorf("创建账户数据目录失败: %w", err)
	}
	file, err := os.OpenFile(m.lockFilePath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开账户存储锁文件失败: %w", err)
	}
	return file, nil
}

func (m *Manager) releaseStore() {
	m.storeHeld.Store(false)
	if m.lockFile != nil {
		unlockFile(m.lockFile)
		_ = m.lockFile.Close()
		m.lockFile = nil
	}
	m.storeMu.Unlock()
}

// lockForWrite 取得存储锁、合并磁盘最新内容并持有 m.mu 写锁，返回按相反顺序释放的函数。
// 写入入口统一用它；合并失败（文件损坏、读取失败）时不修改内存也不写入。
func (m *Manager) lockForWrite(ctx context.Context) (func(), error) {
	ctx, cancel := m.boundedWait(ctx)
	defer cancel()
	if err := m.acquireStore(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if err := m.syncFromDiskLocked(); err != nil {
		m.mu.Unlock()
		m.releaseStore()
		return nil, err
	}
	return func() {
		m.mu.Unlock()
		m.releaseStore()
	}, nil
}

// syncStore 取得存储锁、合并磁盘最新内容后立即释放，供交换前的重新读取使用。
func (m *Manager) syncStore(ctx context.Context) error {
	ctx, cancel := m.boundedWait(ctx)
	defer cancel()
	if err := m.acquireStore(ctx); err != nil {
		return err
	}
	defer m.releaseStore()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.syncFromDiskLocked()
}

// acquireRefreshLease 取得账户级跨进程刷新锁：同一账户的令牌交换在所有实例间串行，
// 但交换期间不占用全局存储锁，暂停、元数据、导入等写入照常进行。进程内由 refreshMu 串行。
// 锁文件名取账户 ID 的摘要，文件内容不含任何凭据。
func (m *Manager) acquireRefreshLease(ctx context.Context, id string) (func(), error) {
	ctx, cancel := m.boundedWait(ctx)
	defer cancel()
	dir := m.path + ".refresh"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建账户刷新锁目录失败: %w", err)
	}
	sum := sha256.Sum256([]byte(id))
	file, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(sum[:8])+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开账户刷新锁文件失败: %w", err)
	}
	if err := waitLockFile(ctx, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		unlockFile(file)
		_ = file.Close()
	}, nil
}

// boundedWait 为没有截止时间的 context 加上存储锁等待上限。
func (m *Manager) boundedWait(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, m.storeWait())
}

// waitLockFile 以 5ms 起、最长 100ms 的间隔重试非阻塞 flock，直到成功或 ctx 结束。
func waitLockFile(ctx context.Context, file *os.File) error {
	delay := 5 * time.Millisecond
	for {
		locked, err := tryLockFile(file)
		if err != nil {
			return fmt.Errorf("锁定 ChatGPT OAuth 账户存储失败: %w", err)
		}
		if locked {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %v", ErrAccountStoreBusy, ctx.Err())
		case <-timer.C:
		}
		if delay < 100*time.Millisecond {
			delay *= 2
		}
	}
}

func (m *Manager) storeWait() time.Duration {
	if m.storeWaitOverride > 0 {
		return m.storeWaitOverride
	}
	return defaultStoreWait
}

// syncShared 是只读路径的机会性同步：最多每秒检查一次文件身份，变化时用非阻塞方式加锁合并。
// 调用方不得持有 m.mu 或存储锁；拿不到锁就跳过，持锁的一方会自己合并。
func (m *Manager) syncShared() {
	interval := sharedSyncInterval
	if m.syncIntervalOverride != 0 {
		interval = m.syncIntervalOverride
	}
	now := time.Now().UnixNano()
	if interval > 0 {
		last := m.lastSharedSync.Load()
		if now-last < int64(interval) || !m.lastSharedSync.CompareAndSwap(last, now) {
			return
		}
	}
	if !m.diskChanged() || !m.tryAcquireStore() {
		return
	}
	m.mu.Lock()
	if err := m.syncFromDiskLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "[system] ChatGPT OAuth account store sync failed: %v\n", err)
	}
	m.mu.Unlock()
	m.releaseStore()
}

// diskChanged 用文件身份、修改时间和大小快速判断账户文件是否被替换。
func (m *Manager) diskChanged() bool {
	info, err := os.Stat(m.path)
	stamp := m.diskStamp.Load()
	if err != nil {
		return stamp != nil && stamp.info != nil
	}
	if stamp == nil || stamp.info == nil {
		return true
	}
	return !os.SameFile(stamp.info, info) || !stamp.info.ModTime().Equal(info.ModTime()) || stamp.info.Size() != info.Size()
}

func (m *Manager) rememberDiskLocked(data []byte) {
	m.diskSum = sha256.Sum256(data)
	m.diskLoaded = true
	if info, err := os.Stat(m.path); err == nil {
		m.diskStamp.Store(&storeStamp{info: info})
	}
}

// syncFromDiskLocked 读取账户文件并合并到内存。调用方持有存储锁与 m.mu 写锁。
// 文件不存在时保留内存：只有本进程首次写入前才会出现，删除全部账户也会写出空列表。
func (m *Manager) syncFromDiskLocked() error {
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取 ChatGPT OAuth 账户文件失败: %w", err)
	}
	sum := sha256.Sum256(data)
	if m.diskLoaded && sum == m.diskSum {
		m.rememberDiskLocked(data)
		return nil
	}
	var store diskStore
	if err := json.Unmarshal(data, &store); err != nil {
		return fmt.Errorf("解析 ChatGPT OAuth 账户文件失败: %w", err)
	}
	m.mergeDiskLocked(store.Accounts)
	m.rememberDiskLocked(data)
	return nil
}

// mergeDiskLocked 以磁盘为准合并账户：新增加入、删除移除、已有账户覆盖可持久化字段并保留运行状态。
func (m *Manager) mergeDiskLocked(disk []*account) {
	initial := !m.diskLoaded
	seen := make(map[string]bool, len(disk))
	changed := false
	for _, incoming := range disk {
		if incoming == nil || incoming.ID == "" || incoming.AccountID == "" || (incoming.AccessToken == "" && incoming.RefreshToken == "") {
			continue
		}
		seen[incoming.ID] = true
		current := m.accounts[incoming.ID]
		if current == nil {
			if incoming.NeedsReauth {
				// 重新授权状态来自文件：恢复告警状态但不重复产生事件，凭据替换后才能产生恢复事件。
				incoming.alertKind = alertReauth
			}
			m.accounts[incoming.ID] = incoming
			changed = true
			continue
		}
		if m.mergeAccountLocked(current, incoming) {
			changed = true
		}
	}
	for id := range m.accounts {
		if !seen[id] {
			// 所有新增入口都在写盘成功后才保留内存项，因此只在磁盘上缺失说明另一实例已删除它。
			delete(m.accounts, id)
			changed = true
		}
	}
	if changed && !initial {
		m.bumpVersionLocked()
	}
}

// credentialSnapshot 是参与凭据世代判断的字段。
type credentialSnapshot struct {
	accountID, idToken, access, refresh string
	expiresAt, lastRefresh              time.Time
	needsReauth                         bool
}

func snapshotCredentials(item *account) credentialSnapshot {
	return credentialSnapshot{
		accountID: item.AccountID, idToken: item.IDToken, access: item.AccessToken, refresh: item.RefreshToken,
		expiresAt: item.ExpiresAt, lastRefresh: item.LastRefresh, needsReauth: item.NeedsReauth,
	}
}

func restoreCredentials(item *account, c credentialSnapshot) {
	item.AccountID, item.IDToken, item.AccessToken, item.RefreshToken = c.accountID, c.idToken, c.access, c.refresh
	item.ExpiresAt, item.LastRefresh, item.NeedsReauth = c.expiresAt, c.lastRefresh, c.needsReauth
}

// controlJSON 是账户控制状态（列表 ETag 覆盖的部分）：不含凭据、用量和刷新时间。
func controlJSON(item *account) []byte {
	data, _ := json.Marshal(struct {
		Email, Plan string
		Tags        []string
		Note        string
		Paused      bool
		PausedAt    *time.Time
		Start, End  *time.Time
	}{item.Email, item.Plan, item.Tags, item.Note, item.Paused, item.PausedAt, item.SubscriptionActiveStart, item.SubscriptionActiveUntil})
	return data
}

// mergeAccountLocked 把磁盘上的可持久化字段合并进内存对象，返回控制状态是否变化。
func (m *Manager) mergeAccountLocked(current, incoming *account) bool {
	beforeControl := controlJSON(current)
	beforeCredentials := snapshotCredentials(current)
	wasPaused, wasReauth := current.Paused, current.NeedsReauth
	unsaved := current.credentialsUnsaved
	copyPersistentFields(current, incoming)
	if unsaved {
		// 本进程刷新成功但未写盘：持锁期间没有其他实例能做更新的交换，内存凭据仍是最新的。
		restoreCredentials(current, beforeCredentials)
	}
	if snapshotCredentials(current) != beforeCredentials {
		current.credentialVersion++
		current.lastRefreshFailure = nil
		// 另一实例的轮换同样是一次成功入口，清零本进程后台令牌失败计数。
		m.noteTokenSuccess(current)
	}
	if current.Paused && !wasPaused {
		wakeAdmissionWatchersLocked(current, ErrAccountPaused)
	}
	switch {
	case current.NeedsReauth && !wasReauth:
		// 另一实例已记录并提示过，这里只同步状态，不重复产生事件。
		current.alertKind = alertReauth
	case !current.NeedsReauth && wasReauth:
		current.alertKind = alertNone
	}
	return !bytes.Equal(beforeControl, controlJSON(current))
}

// copyPersistentFields 复制 account 的全部导出字段（即账户文件里的字段），不触碰锁、监听与运行状态。
// 用反射而不是逐项赋值，新增持久化字段时不会漏同步。
func copyPersistentFields(dst, src *account) {
	dv, sv := reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem()
	t := dv.Type()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).IsExported() {
			dv.Field(i).Set(sv.Field(i))
		}
	}
}
