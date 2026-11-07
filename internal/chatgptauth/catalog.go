package chatgptauth

import (
	"context"
	"net/http"
)

// CatalogIdentity 是仅供进程内目录缓存使用的身份，不包含凭据且不可序列化。
// 账户指针区分删除后以相同 ID 重建的账户，version 隔离同一对象上的凭据替换。
type CatalogIdentity struct {
	account *account
	version uint64
}

// CatalogIdentity 返回账户的当前缓存身份，不刷新凭据。
func (m *Manager) CatalogIdentity(id string) (CatalogIdentity, error) {
	m.syncShared()
	m.mu.RLock()
	defer m.mu.RUnlock()
	item := m.accounts[id]
	if item == nil {
		return CatalogIdentity{}, ErrAccountNotFound
	}
	if item.NeedsReauth {
		return CatalogIdentity{}, ErrReauthorizationRequired
	}
	return CatalogIdentity{account: item, version: item.credentialVersion}, nil
}

// CatalogIdentityCurrent 判断目录结果是否仍属于当前账户的凭据世代。
func (m *Manager) CatalogIdentityCurrent(id string, identity CatalogIdentity) bool {
	current, err := m.CatalogIdentity(id)
	return err == nil && current == identity
}

// CatalogCredentials 复用现有刷新锁与安全落盘，并在同一读锁内捕获令牌对应的身份。
// 获取后凭据若又被替换，调用方须在返回目录和写缓存前用 CatalogIdentityCurrent 复核。
// 管理暂停不阻止目录查询，目录查询也不会记作模型调用。
func (m *Manager) CatalogCredentials(ctx context.Context, id string, client *http.Client) (token, remoteID string, identity CatalogIdentity, err error) {
	m.mu.RLock()
	original := m.accounts[id]
	m.mu.RUnlock()
	if original == nil {
		return "", "", CatalogIdentity{}, ErrAccountNotFound
	}
	token, remoteID, err = m.AccessToken(ctx, id, client)
	if err != nil {
		return "", "", CatalogIdentity{}, err
	}
	if err := ctx.Err(); err != nil {
		return "", "", CatalogIdentity{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	item := m.accounts[id]
	if item != original || item == nil || item.AccessToken != token || item.AccountID != remoteID {
		return "", "", CatalogIdentity{}, ErrCredentialsReplaced
	}
	if item.NeedsReauth {
		return "", "", CatalogIdentity{}, ErrReauthorizationRequired
	}
	return token, remoteID, CatalogIdentity{account: item, version: item.credentialVersion}, nil
}
