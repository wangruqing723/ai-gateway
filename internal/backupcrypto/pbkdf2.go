package backupcrypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// pbkdf2SHA256 按 RFC 8018 第 5.2 节实现 PBKDF2，伪随机函数为 HMAC-SHA256：
//
//	DK = T_1 || T_2 || ... || T_l（截断到 keyLen 字节）
//	T_i = U_1 ^ U_2 ^ ... ^ U_c
//	U_1 = PRF(P, S || INT(i))，U_j = PRF(P, U_{j-1})
//
// 调用方负责保证 iterations ≥ 1、keyLen ≥ 1；本包只以 KeySize 调用，远小于
// RFC 规定的 (2^32 - 1) * hLen 上限。中间缓冲在返回前清零（尽力而为，见 newAEAD 说明）。
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen

	dk := make([]byte, 0, blocks*hLen)
	u := make([]byte, 0, hLen)
	t := make([]byte, hLen)
	var counter [4]byte
	for block := 1; block <= blocks; block++ {
		binary.BigEndian.PutUint32(counter[:], uint32(block))
		prf.Reset()
		prf.Write(salt)
		prf.Write(counter[:])
		u = prf.Sum(u[:0])
		copy(t, u)
		for n := 2; n <= iterations; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for x := range t {
				t[x] ^= u[x]
			}
		}
		dk = append(dk, t...)
	}
	clear(u[:cap(u)])
	clear(t)

	key := make([]byte, keyLen)
	copy(key, dk)
	clear(dk[:cap(dk)])
	return key
}
