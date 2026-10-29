// Package store 用 bbolt 持久化 relay 的路由表、配对码、待审批与收件箱。
//
// 单实例 + 单写者：bbolt 的写事务天然串行，避免多写者竞争。
package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/twotwoba/DevPilot-APNS/internal/pairing"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

// SchemaVersion 是 bbolt 数据布局版本。
//
// v2(2026-10):Daemon 增 Tier(配额档位)与 LastSeenAt(TTL 清理依据),
// 无新 bucket;旧 JSON 字段向后兼容。
const SchemaVersion = 2

// 容量与 TTL（与 pkg/wire 的超时常量保持同一量级）。
const (
	MaxInboxPerDaemon = 1000
	InboxTTL          = 24 * time.Hour
	EventIDTTL        = 10 * time.Minute
	PendingGrace      = 10 * time.Minute
	PairingCodeGrace  = time.Hour
)

var (
	// ErrNotFound 表示记录不存在。
	ErrNotFound = errors.New("store: record not found")
	// ErrCodeInvalid 表示配对码不存在或已被使用。
	ErrCodeInvalid = errors.New("store: pairing code invalid or already used")
	// ErrCodeExpired 表示配对码已过期。
	ErrCodeExpired = errors.New("store: pairing code expired")
)

var (
	bucketMeta     = []byte("meta")
	bucketDaemons  = []byte("daemons")
	bucketCodes    = []byte("pairing_codes")
	bucketDevices  = []byte("devices")
	bucketPending  = []byte("pending")
	bucketInbox    = []byte("inbox")
	bucketEventIDs = []byte("event_ids")
	bucketLATokens = []byte("la_tokens")
)

// Daemon 是一台已注册的开发机。
type Daemon struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CredHash  string `json:"cred_hash"`
	CreatedAt int64  `json:"created_at"`
	LastSeq   int64  `json:"last_seq"`
	// Tier 是配额档位,App 配对时提交(v1 阶段客户端自声明,收据验证属后续硬化)。
	Tier Tier `json:"tier,omitempty"`
	// LastSeenAt 是 daemon 侧最近活跃时刻(WSS 建连/登记配对会话时刷新),TTL 清理依据。
	LastSeenAt int64 `json:"last_seen_at,omitempty"`
}

// Tier 是 daemon 的配额档位。
type Tier string

const (
	TierFree Tier = "free"
	TierPro  Tier = "pro"
)

// Valid 报告档位值是否合法。
func (t Tier) Valid() bool { return t == TierFree || t == TierPro }

// ParseTier 解析客户端提交的档位;空值视为 free,未识别值返回 ok=false(调用方应 400)。
func ParseTier(s string) (Tier, bool) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", string(TierFree):
		return TierFree, true
	case string(TierPro):
		return TierPro, true
	default:
		return "", false
	}
}

// PairingSession 是待 App 兑换的配对码。
type PairingSession struct {
	CodeHash   string `json:"code_hash"`
	DaemonID   string `json:"daemon_id"`
	DaemonName string `json:"daemon_name"`
	CredHash   string `json:"cred_hash"`
	ExpiresAt  int64  `json:"expires_at"`
	CreatedAt  int64  `json:"created_at"`
}

// Device 是一台已配对的 iPhone。
type Device struct {
	ID         string `json:"id"`
	DaemonID   string `json:"daemon_id"`
	CredHash   string `json:"cred_hash"`
	APNSToken  string `json:"apns_token,omitempty"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt int64  `json:"last_seen_at"`
}

// Pending 是一次待审批（或已决策待投递）的审批。
type Pending struct {
	ApprovalID   string        `json:"approval_id"`
	DaemonID     string        `json:"daemon_id"`
	Event        wire.Envelope `json:"event"`
	Status       string        `json:"status"` // pending / approved / denied / expired / cancelled
	Decision     string        `json:"decision,omitempty"`
	CreatedAt    int64         `json:"created_at"`
	ExpiresAt    int64         `json:"expires_at"`
	DecidedAt    int64         `json:"decided_at,omitempty"`
	DeliverUntil int64         `json:"deliver_until,omitempty"`
	Delivered    bool          `json:"delivered,omitempty"`
}

// LAToken 是某个 (device, session) 的 Live Activity push token。
type LAToken struct {
	DeviceID  string `json:"device_id"`
	DaemonID  string `json:"daemon_id"`
	SessionID string `json:"session_id"`
	PushToken string `json:"push_token"`
	UpdatedAt int64  `json:"updated_at"`
}

// Store 封装 bbolt。
type Store struct {
	db   *bolt.DB
	path string
}

// Open 打开（或创建）数据库并初始化 bucket。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: failed to create data directory: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: failed to open database: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{
			bucketMeta, bucketDaemons, bucketCodes, bucketDevices,
			bucketPending, bucketInbox, bucketEventIDs, bucketLATokens,
		} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get([]byte("schema_version")); raw == nil {
			// 新库:直接落当前版本。
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], SchemaVersion)
			if err := meta.Put([]byte("schema_version"), buf[:]); err != nil {
				return err
			}
		} else if v := binary.BigEndian.Uint64(raw); v < SchemaVersion {
			// 旧库:逐级迁移后改版本号。v0.1.0 二进制只判「版本键存在」不校验值,
			// 因此 v2 库可被旧版直接打开(新增 JSON 字段被忽略),二进制回滚不锁库。
			if v < 2 {
				if err := migrateV1ToV2(tx); err != nil {
					return err
				}
			}
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], SchemaVersion)
			if err := meta.Put([]byte("schema_version"), buf[:]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: failed to initialize: %w", err)
	}
	return s, nil
}

// migrateV1ToV2 为存量 Daemon 补 Tier 与 LastSeenAt。
//
// LastSeenAt 必须回填 CreatedAt:不回填则升级当天全部 daemon 的活跃时刻为 0,
// 会被 TTL 清理误判「长期不活跃」而整档删除。
func migrateV1ToV2(tx *bolt.Tx) error {
	daemons := tx.Bucket(bucketDaemons)
	return daemons.ForEach(func(k, v []byte) error {
		var d Daemon
		if err := json.Unmarshal(v, &d); err != nil {
			return err
		}
		changed := false
		if d.Tier == "" {
			d.Tier = TierFree
			changed = true
		}
		if d.LastSeenAt == 0 {
			d.LastSeenAt = d.CreatedAt
			changed = true
		}
		if changed {
			return putJSON(daemons, string(k), d)
		}
		return nil
	})
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// Ping 做一次只读事务，供 /readyz 使用。
func (s *Store) Ping() error {
	return s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketMeta) == nil {
			return errors.New("store: meta bucket missing")
		}
		return nil
	})
}

// Snapshot 把数据库一致性快照写到 out（先写临时文件再原子替换）。
func (s *Store) Snapshot(out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return err
	}
	tmp := out + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = s.db.View(func(tx *bolt.Tx) error { _, err := tx.WriteTo(f); return err })
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}

// ---------- daemon / pairing ----------

// RegisterPairingSession 写入/轮换 daemon 记录并登记一次性配对码。
//
// 调用方必须先完成「已有 daemon 的凭据校验」，本方法不再做鉴权。
func (s *Store) RegisterPairingSession(ps PairingSession) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		daemons := tx.Bucket(bucketDaemons)
		existing := daemons.Get([]byte(ps.DaemonID))
		rec := Daemon{
			ID:         ps.DaemonID,
			Name:       ps.DaemonName,
			CredHash:   ps.CredHash,
			CreatedAt:  ps.CreatedAt,
			Tier:       TierFree,
			LastSeenAt: ps.CreatedAt,
		}
		if existing != nil {
			if err := json.Unmarshal(existing, &rec); err != nil {
				return err
			}
			rec.Name = ps.DaemonName
			rec.CredHash = ps.CredHash
			// 轮换凭据即 daemon 侧活跃,顺带续 TTL。
			rec.LastSeenAt = ps.CreatedAt
		}
		if err := putJSON(daemons, ps.DaemonID, rec); err != nil {
			return err
		}
		return putJSON(tx.Bucket(bucketCodes), ps.CodeHash, ps)
	})
}

// Daemon 读取一台开发机。
func (s *Store) Daemon(id string) (Daemon, error) {
	var d Daemon
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketDaemons), id, &d)
	})
	return d, err
}

// VerifyDaemon 校验 daemon 凭据；成功返回记录。
func (s *Store) VerifyDaemon(id, secret string) (Daemon, bool) {
	d, err := s.Daemon(id)
	if err != nil || !pairing.VerifyHash(secret, d.CredHash) {
		return Daemon{}, false
	}
	return d, true
}

// Pair 用配对码兑换 device 凭据；配对码一次性焚毁。
//
// 同时把 App 提交的档位写入 daemon（配额分层的事实源）；
// 同一 daemon 只保留一台 device（重新配对会顶掉旧设备）。
func (s *Store) Pair(codeHash string, device Device, tier Tier, now int64) (Daemon, Device, error) {
	var daemon Daemon
	err := s.db.Update(func(tx *bolt.Tx) error {
		codes := tx.Bucket(bucketCodes)
		raw := codes.Get([]byte(codeHash))
		if raw == nil {
			return ErrCodeInvalid
		}
		var ps PairingSession
		if err := json.Unmarshal(raw, &ps); err != nil {
			return err
		}
		if now > ps.ExpiresAt {
			_ = codes.Delete([]byte(codeHash))
			return ErrCodeExpired
		}
		if err := getJSON(tx.Bucket(bucketDaemons), ps.DaemonID, &daemon); err != nil {
			return ErrCodeInvalid
		}
		if !tier.Valid() {
			tier = TierFree
		}
		daemon.Tier = tier
		if err := putJSON(tx.Bucket(bucketDaemons), ps.DaemonID, daemon); err != nil {
			return err
		}
		devices := tx.Bucket(bucketDevices)
		var stale [][]byte
		if err := devices.ForEach(func(k, v []byte) error {
			var d Device
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			if d.DaemonID == ps.DaemonID {
				stale = append(stale, bytes.Clone(k))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range stale {
			// 走 deleteDeviceTx 级联清 LA token：只删 device 行会留下
			// 永远无人读取的孤儿 token(设备行已不在,读取路径全部按行走)。
			if err := deleteDeviceTx(tx, string(k)); err != nil {
				return err
			}
		}
		device.DaemonID = ps.DaemonID
		device.CreatedAt = now
		device.LastSeenAt = now
		if err := putJSON(devices, device.ID, device); err != nil {
			return err
		}
		return codes.Delete([]byte(codeHash))
	})
	return daemon, device, err
}

// Device 读取一台设备。
func (s *Store) Device(id string) (Device, error) {
	var d Device
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketDevices), id, &d)
	})
	return d, err
}

// VerifyDevice 校验 device 凭据并更新 last_seen。
func (s *Store) VerifyDevice(id, secret string) (Device, bool) {
	d, err := s.Device(id)
	if err != nil || !pairing.VerifyHash(secret, d.CredHash) {
		return Device{}, false
	}
	return d, true
}

// TouchDevice 更新 last_seen_at。
func (s *Store) TouchDevice(id string, now int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var d Device
		if err := getJSON(tx.Bucket(bucketDevices), id, &d); err != nil {
			return err
		}
		d.LastSeenAt = now
		return putJSON(tx.Bucket(bucketDevices), id, d)
	})
}

// ClearDeviceAPNSToken 在 APNs 返回 410 后清空失效的 device token。
func (s *Store) ClearDeviceAPNSToken(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var d Device
		if err := getJSON(tx.Bucket(bucketDevices), id, &d); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		d.APNSToken = ""
		return putJSON(tx.Bucket(bucketDevices), id, d)
	})
}

// TouchDaemon 更新 daemon 的 last_seen_at(WSS 建连等 daemon 侧活跃时调用)。
func (s *Store) TouchDaemon(id string, now int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var d Daemon
		if err := getJSON(tx.Bucket(bucketDaemons), id, &d); err != nil {
			return err
		}
		d.LastSeenAt = now
		return putJSON(tx.Bucket(bucketDaemons), id, d)
	})
}

// SetDaemonTier 覆写 daemon 档位(App 权益翻转后经 token 端点上报)。
// 配额按 Conn.Tier 结算、建连时缓存，改档后由 server 踢连接促重连生效。
func (s *Store) SetDaemonTier(id string, tier Tier, now int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var d Daemon
		if err := getJSON(tx.Bucket(bucketDaemons), id, &d); err != nil {
			return err
		}
		d.Tier = tier
		d.LastSeenAt = now
		return putJSON(tx.Bucket(bucketDaemons), id, d)
	})
}

// UpdateDeviceAPNSToken 覆写设备的 APNs token(App 重新上报/410 后补报)。
func (s *Store) UpdateDeviceAPNSToken(id, token string, now int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var d Device
		if err := getJSON(tx.Bucket(bucketDevices), id, &d); err != nil {
			return err
		}
		d.APNSToken = token
		d.LastSeenAt = now
		return putJSON(tx.Bucket(bucketDevices), id, d)
	})
}

// DevicesForDaemon 返回一台 daemon 绑定的所有设备。
func (s *Store) DevicesForDaemon(daemonID string) ([]Device, error) {
	var out []Device
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketDevices).ForEach(func(_, v []byte) error {
			var d Device
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			if d.DaemonID == daemonID {
				out = append(out, d)
			}
			return nil
		})
	})
	return out, err
}

// DeleteDevice 删除一台设备及其全部 LA token(设备自助解绑)。
func (s *Store) DeleteDevice(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return deleteDeviceTx(tx, id)
	})
}

// DeleteDaemonCascade 吊销一台 daemon 及其全部从属数据
// (devices/la_tokens/inbox/event_ids/pending),unpair 与 TTL 清理共用。
func (s *Store) DeleteDaemonCascade(daemonID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return deleteDaemonTx(tx, daemonID)
	})
}

func deleteDeviceTx(tx *bolt.Tx, deviceID string) error {
	if err := deletePrefix(tx.Bucket(bucketLATokens), []byte(deviceID+"/")); err != nil {
		return err
	}
	return tx.Bucket(bucketDevices).Delete([]byte(deviceID))
}

func deleteDaemonTx(tx *bolt.Tx, daemonID string) error {
	if err := tx.Bucket(bucketDaemons).Delete([]byte(daemonID)); err != nil {
		return err
	}
	// 收件箱与事件去重表按 daemonID 前缀存储,前缀扫删。
	if err := deletePrefix(tx.Bucket(bucketInbox), []byte(daemonID+"/")); err != nil {
		return err
	}
	if err := deletePrefix(tx.Bucket(bucketEventIDs), []byte(daemonID+"/")); err != nil {
		return err
	}
	// devices / la_tokens / pending 按 DaemonID 字段清扫。
	var devKeys, laKeys, pendingKeys [][]byte
	if err := tx.Bucket(bucketDevices).ForEach(func(k, v []byte) error {
		var d Device
		if err := json.Unmarshal(v, &d); err != nil {
			return err
		}
		if d.DaemonID == daemonID {
			devKeys = append(devKeys, bytes.Clone(k))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := tx.Bucket(bucketLATokens).ForEach(func(k, v []byte) error {
		var t LAToken
		if err := json.Unmarshal(v, &t); err != nil {
			return err
		}
		if t.DaemonID == daemonID {
			laKeys = append(laKeys, bytes.Clone(k))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := tx.Bucket(bucketPending).ForEach(func(k, v []byte) error {
		var p Pending
		if err := json.Unmarshal(v, &p); err != nil {
			return err
		}
		if p.DaemonID == daemonID {
			pendingKeys = append(pendingKeys, bytes.Clone(k))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, target := range []struct {
		bucket *bolt.Bucket
		keys   [][]byte
	}{
		{tx.Bucket(bucketDevices), devKeys},
		{tx.Bucket(bucketLATokens), laKeys},
		{tx.Bucket(bucketPending), pendingKeys},
	} {
		for _, k := range target.keys {
			if err := target.bucket.Delete(k); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------- 收件箱 ----------

// AppendInbox 为事件分配 seq 并写入收件箱；重复 event_id 直接返回已有 seq。
func (s *Store) AppendInbox(daemonID string, env wire.Envelope, now int64) (wire.Envelope, bool, error) {
	var duplicate bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		ids := tx.Bucket(bucketEventIDs)
		key := eventIDKey(daemonID, env.EventID)
		if raw := ids.Get(key); raw != nil {
			var rec eventIDRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			env.Seq = rec.Seq
			duplicate = true
			return nil
		}

		daemons := tx.Bucket(bucketDaemons)
		var d Daemon
		if err := getJSON(daemons, daemonID, &d); err != nil {
			return err
		}
		d.LastSeq++
		env.Seq = d.LastSeq
		if err := putJSON(daemons, daemonID, d); err != nil {
			return err
		}

		inbox := tx.Bucket(bucketInbox)
		entry := inboxEntry{Envelope: env, CreatedAt: now}
		if err := putJSONKey(inbox, inboxKey(daemonID, env.Seq), entry); err != nil {
			return err
		}
		if err := putJSONKey(ids, key, eventIDRecord{Seq: env.Seq, CreatedAt: now}); err != nil {
			return err
		}
		return trimInbox(tx, daemonID)
	})
	return env, duplicate, err
}

// InboxSince 返回 afterSeq 之后、且 TS >= since 的收件箱条目。
//
// limit 控制单次返回量，next 为下一页游标（0 表示没有更多）。
func (s *Store) InboxSince(daemonID string, afterSeq, since int64, limit int) ([]wire.Envelope, int64, error) {
	if limit <= 0 {
		limit = 200
	}
	var out []wire.Envelope
	var next int64
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketInbox).Cursor()
		prefix := []byte(daemonID + "/")
		k, v := c.Seek(prefix)
		for ; k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var entry inboxEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return err
			}
			if entry.Envelope.Seq <= afterSeq {
				continue
			}
			if since > 0 && entry.Envelope.TS < since {
				continue
			}
			if len(out) >= limit {
				next = out[len(out)-1].Seq
				return nil
			}
			out = append(out, entry.Envelope)
		}
		return nil
	})
	return out, next, err
}

// ---------- 审批 ----------

// PutPending 登记一次待审批。
func (s *Store) PutPending(p Pending) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if existing := tx.Bucket(bucketPending).Get([]byte(p.ApprovalID)); existing != nil {
			return nil // 幂等：同 approval_id 只保留第一次
		}
		return putJSON(tx.Bucket(bucketPending), p.ApprovalID, p)
	})
}

// GetPending 读取审批记录。
func (s *Store) GetPending(approvalID string) (Pending, error) {
	var p Pending
	err := s.db.View(func(tx *bolt.Tx) error {
		return getJSON(tx.Bucket(bucketPending), approvalID, &p)
	})
	return p, err
}

// PendingForDaemon 返回仍在等待中的审批。
func (s *Store) PendingForDaemon(daemonID string, now int64) ([]Pending, error) {
	var out []Pending
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPending).ForEach(func(_, v []byte) error {
			var p Pending
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if p.DaemonID == daemonID && p.Status == "pending" && now <= p.ExpiresAt {
				out = append(out, p)
			}
			return nil
		})
	})
	return out, err
}

// ApplyDecision 以 first-wins 语义写入决策，并返回 ack 状态。
func (s *Store) ApplyDecision(approvalID, daemonID, decision string, now int64) (wire.AckStatus, Pending, error) {
	var (
		status wire.AckStatus
		out    Pending
	)
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketPending)
		raw := bucket.Get([]byte(approvalID))
		if raw == nil {
			status = wire.AckUnknown
			return nil
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return err
		}
		// 不同 daemon 之间不泄露审批是否存在。
		if out.DaemonID != daemonID {
			status = wire.AckUnknown
			out = Pending{}
			return nil
		}
		switch out.Status {
		case "pending":
			if now > out.ExpiresAt {
				out.Status = "expired"
				status = wire.AckExpired
				return putJSON(bucket, approvalID, out)
			}
			out.Status = statusForDecision(decision)
			out.Decision = decision
			out.DecidedAt = now
			out.DeliverUntil = now + int64(PendingGrace.Seconds())
			status = wire.AckAccepted
			return putJSON(bucket, approvalID, out)
		default:
			status = wire.AckAlreadyDecided
			if out.Status == "expired" || out.Status == "cancelled" {
				status = wire.AckExpired
			}
			return nil
		}
	})
	return status, out, err
}

// UndeliveredDecisions 返回已决策但 daemon 尚未确认的审批。
func (s *Store) UndeliveredDecisions(daemonID string, now int64) ([]Pending, error) {
	var out []Pending
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPending).ForEach(func(_, v []byte) error {
			var p Pending
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if p.DaemonID != daemonID || p.Delivered {
				return nil
			}
			if p.Status != "approved" && p.Status != "denied" {
				return nil
			}
			if now > p.DeliverUntil {
				return nil
			}
			out = append(out, p)
			return nil
		})
	})
	return out, err
}

// MarkDecisionDelivered 在 daemon 回 ack 后标记决策已送达。
func (s *Store) MarkDecisionDelivered(approvalID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketPending)
		var p Pending
		if err := getJSON(bucket, approvalID, &p); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		p.Delivered = true
		return putJSON(bucket, approvalID, p)
	})
}

// CountPending 统计当前等待中的审批数（/metrics 用）。
func (s *Store) CountPending(now int64) (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPending).ForEach(func(_, v []byte) error {
			var p Pending
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			if p.Status == "pending" && now <= p.ExpiresAt {
				n++
			}
			return nil
		})
	})
	return n, err
}

// ---------- Live Activity token ----------

// PutLAToken 写入/覆盖一个 Live Activity token。
func (s *Store) PutLAToken(t LAToken) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return putJSONKey(tx.Bucket(bucketLATokens), laKey(t.DeviceID, t.SessionID), t)
	})
}

// DeleteLAToken 删除一个 Live Activity token（会话结束时）。
func (s *Store) DeleteLAToken(deviceID, sessionID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketLATokens).Delete(laKey(deviceID, sessionID))
	})
}

// LATokensForDaemon 返回一台 daemon 下所有 LA token（APNs 转发用）。
func (s *Store) LATokensForDaemon(daemonID string) ([]LAToken, error) {
	var out []LAToken
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketLATokens).ForEach(func(_, v []byte) error {
			var t LAToken
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			if t.DaemonID == daemonID {
				out = append(out, t)
			}
			return nil
		})
	})
	return out, err
}

// ---------- 清理 ----------

// Cleanup 执行 TTL 清理；由 relay 的后台协程周期调用。
func (s *Store) Cleanup(now int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := cleanupPending(tx, now); err != nil {
			return err
		}
		if err := cleanupInbox(tx, now); err != nil {
			return err
		}
		if err := cleanupCodes(tx, now); err != nil {
			return err
		}
		return cleanupLATokens(tx, now)
	})
}

func cleanupPending(tx *bolt.Tx, now int64) error {
	b := tx.Bucket(bucketPending)
	var remove [][]byte
	if err := b.ForEach(func(k, v []byte) error {
		var p Pending
		if err := json.Unmarshal(v, &p); err != nil {
			return err
		}
		// 过期审批先标记 expired 并保留一段时间，供 App 对账看到最终状态。
		if p.Status == "pending" && now > p.ExpiresAt {
			p.Status = "expired"
			if err := putJSON(b, string(k), p); err != nil {
				return err
			}
			return nil
		}
		if now > p.ExpiresAt+int64(PendingGrace.Seconds()) {
			remove = append(remove, bytes.Clone(k))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, k := range remove {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

func cleanupInbox(tx *bolt.Tx, now int64) error {
	inbox := tx.Bucket(bucketInbox)
	ids := tx.Bucket(bucketEventIDs)
	ttl := int64(InboxTTL.Seconds())
	eventTTL := int64(EventIDTTL.Seconds())
	var removeInbox, removeIDs [][]byte
	if err := inbox.ForEach(func(k, v []byte) error {
		var entry inboxEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return err
		}
		if now-entry.CreatedAt > ttl {
			removeInbox = append(removeInbox, bytes.Clone(k))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := ids.ForEach(func(k, v []byte) error {
		var rec eventIDRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return err
		}
		if now-rec.CreatedAt > eventTTL {
			removeIDs = append(removeIDs, bytes.Clone(k))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, k := range removeInbox {
		if err := inbox.Delete(k); err != nil {
			return err
		}
	}
	for _, k := range removeIDs {
		if err := ids.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

func cleanupCodes(tx *bolt.Tx, now int64) error {
	b := tx.Bucket(bucketCodes)
	var remove [][]byte
	if err := b.ForEach(func(k, v []byte) error {
		var ps PairingSession
		if err := json.Unmarshal(v, &ps); err != nil {
			return err
		}
		if now > ps.ExpiresAt+int64(PairingCodeGrace.Seconds()) {
			remove = append(remove, bytes.Clone(k))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, k := range remove {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

func cleanupLATokens(tx *bolt.Tx, now int64) error {
	b := tx.Bucket(bucketLATokens)
	var remove [][]byte
	if err := b.ForEach(func(k, v []byte) error {
		var t LAToken
		if err := json.Unmarshal(v, &t); err != nil {
			return err
		}
		if now-t.UpdatedAt > int64(InboxTTL.Seconds()) {
			remove = append(remove, bytes.Clone(k))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, k := range remove {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// CleanupIdentities 清理超期未活跃的 daemon(级联)与 device(含 LA token),
// 托管模式的生命周期回收;TTL 为 0 表示对应维度禁用。返回清理数量供日志。
func (s *Store) CleanupIdentities(now int64, daemonTTL, deviceTTL time.Duration) (removedDaemons, removedDevices int, err error) {
	if daemonTTL <= 0 && deviceTTL <= 0 {
		return 0, 0, nil
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		if deviceTTL > 0 {
			devices := tx.Bucket(bucketDevices)
			var stale []string
			if scanErr := devices.ForEach(func(k, v []byte) error {
				var d Device
				if json.Unmarshal(v, &d) != nil {
					return nil // 坏行不阻断清理
				}
				if d.LastSeenAt > 0 && now-d.LastSeenAt > int64(deviceTTL.Seconds()) {
					stale = append(stale, string(k))
				}
				return nil
			}); scanErr != nil {
				return scanErr
			}
			for _, id := range stale {
				if delErr := deleteDeviceTx(tx, id); delErr != nil {
					return delErr
				}
				removedDevices++
			}
		}
		if daemonTTL > 0 {
			daemons := tx.Bucket(bucketDaemons)
			var stale []string
			if scanErr := daemons.ForEach(func(k, v []byte) error {
				var d Daemon
				if json.Unmarshal(v, &d) != nil {
					return nil // 坏行不阻断清理
				}
				if d.LastSeenAt > 0 && now-d.LastSeenAt > int64(daemonTTL.Seconds()) {
					stale = append(stale, string(k))
				}
				return nil
			}); scanErr != nil {
				return scanErr
			}
			for _, id := range stale {
				if delErr := deleteDaemonTx(tx, id); delErr != nil {
					return delErr
				}
				removedDaemons++
			}
		}
		return nil
	})
	return removedDaemons, removedDevices, err
}

// trimInbox 在写入后维持每 daemon 的条数上限。
func trimInbox(tx *bolt.Tx, daemonID string) error {
	inbox := tx.Bucket(bucketInbox)
	c := inbox.Cursor()
	prefix := []byte(daemonID + "/")
	var keys [][]byte
	k, _ := c.Seek(prefix)
	for ; k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
		keys = append(keys, bytes.Clone(k))
	}
	excess := len(keys) - MaxInboxPerDaemon
	for i, key := range keys {
		if i < excess {
			if err := inbox.Delete(key); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------- 编码辅助 ----------

type inboxEntry struct {
	Envelope  wire.Envelope `json:"envelope"`
	CreatedAt int64         `json:"created_at"`
}

type eventIDRecord struct {
	Seq       int64 `json:"seq"`
	CreatedAt int64 `json:"created_at"`
}

func inboxKey(daemonID string, seq int64) []byte {
	return fmt.Appendf(nil, "%s/%020d", daemonID, seq)
}

func eventIDKey(daemonID, eventID string) []byte {
	return []byte(daemonID + "/" + eventID)
}

func laKey(deviceID, sessionID string) []byte {
	return []byte(deviceID + "/" + sessionID)
}

// deletePrefix 删除 bucket 中带前缀的全部 key
// (bbolt 游标序即字典序;先收集再删,避免删除时移动游标)。
func deletePrefix(b *bolt.Bucket, prefix []byte) error {
	c := b.Cursor()
	var keys [][]byte
	for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
		keys = append(keys, bytes.Clone(k))
	}
	for _, k := range keys {
		if err := b.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// statusForDecision 把线上决策值映射为内部审批状态。
func statusForDecision(decision string) string {
	switch decision {
	case "allow":
		return "approved"
	case "deny":
		return "denied"
	default:
		return decision
	}
}

func putJSON(b *bolt.Bucket, key string, v any) error {
	return putJSONKey(b, []byte(key), v)
}

func putJSONKey(b *bolt.Bucket, key []byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Put(key, data)
}

func getJSON(b *bolt.Bucket, key string, v any) error {
	raw := b.Get([]byte(key))
	if raw == nil {
		return ErrNotFound
	}
	return json.Unmarshal(raw, v)
}
