package ledis

import (
	"encoding/binary"
	"errors"
	"github.com/siddontang/ledisdb/leveldb"
	"time"
)

const (
	listHeadSeq int32 = 1
	listTailSeq int32 = 2

	listMinSeq     int32 = 1000
	listMaxSeq     int32 = 1<<31 - 1000
	listInitialSeq int32 = listMinSeq + (listMaxSeq-listMinSeq)/2
)

var errLMetaKey = errors.New("invalid lmeta key")
var errListKey = errors.New("invalid list key")
var errListSeq = errors.New("invalid list sequence, overflow")

func (db *DB) lEncodeMetaKey(key []byte) []byte {
	buf := make([]byte, len(key)+2)
	buf[0] = db.index
	buf[1] = lMetaType

	copy(buf[2:], key)
	return buf
}

func (db *DB) lDecodeMetaKey(ek []byte) ([]byte, error) {
	if len(ek) < 2 || ek[0] != db.index || ek[1] != lMetaType {
		return nil, errLMetaKey
	}

	return ek[2:], nil
}

func (db *DB) lEncodeListKey(key []byte, seq int32) []byte {
	buf := make([]byte, len(key)+8)

	pos := 0
	buf[pos] = db.index
	pos++
	buf[pos] = listType
	pos++

	binary.BigEndian.PutUint16(buf[pos:], uint16(len(key)))
	pos += 2

	copy(buf[pos:], key)
	pos += len(key)

	binary.BigEndian.PutUint32(buf[pos:], uint32(seq))

	return buf
}

func (db *DB) lDecodeListKey(ek []byte) (key []byte, seq int32, err error) {
	if len(ek) < 8 || ek[0] != db.index || ek[1] != listType {
		err = errListKey
		return
	}

	keyLen := int(binary.BigEndian.Uint16(ek[2:]))
	if keyLen+8 != len(ek) {
		err = errListKey
		return
	}

	key = ek[4 : 4+keyLen]
	seq = int32(binary.BigEndian.Uint32(ek[4+keyLen:]))
	return
}

func (db *DB) lpush(key []byte, whereSeq int32, args ...[]byte) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}

	var headSeq int32
	var tailSeq int32
	var size int32
	var err error

	metaKey := db.lEncodeMetaKey(key)
	headSeq, tailSeq, size, err = db.lGetMeta(metaKey)
	if err != nil {
		return 0, err
	}

	var pushCnt int = len(args)
	if pushCnt == 0 {
		return int64(size), nil
	}

	var seq int32 = headSeq
	var delta int32 = -1
	if whereSeq == listTailSeq {
		seq = tailSeq
		delta = 1
	}

	t := db.listTx
	t.Lock()
	defer t.Unlock()

	//	append elements
	if size > 0 {
		seq += delta
	}

	for i := 0; i < pushCnt; i++ {
		ek := db.lEncodeListKey(key, seq+int32(i)*delta)
		t.Put(ek, args[i])
	}

	seq += int32(pushCnt-1) * delta
	if seq <= listMinSeq || seq >= listMaxSeq {
		return 0, errListSeq
	}

	//	set meta info
	if whereSeq == listHeadSeq {
		headSeq = seq
	} else {
		tailSeq = seq
	}

	db.lSetMeta(metaKey, headSeq, tailSeq)

	err = t.Commit()
	return int64(size) + int64(pushCnt), err
}

func (db *DB) lpop(key []byte, whereSeq int32) ([]byte, error) {
	if err := checkKeySize(key); err != nil {
		return nil, err
	}

	t := db.listTx
	t.Lock()
	defer t.Unlock()

	var headSeq int32
	var tailSeq int32
	var err error

	metaKey := db.lEncodeMetaKey(key)
	headSeq, tailSeq, _, err = db.lGetMeta(metaKey)
	if err != nil {
		return nil, err
	}

	var value []byte

	var seq int32 = headSeq
	if whereSeq == listTailSeq {
		seq = tailSeq
	}

	itemKey := db.lEncodeListKey(key, seq)
	value, err = db.db.Get(itemKey)
	if err != nil {
		return nil, err
	}

	if whereSeq == listHeadSeq {
		headSeq += 1
	} else {
		tailSeq -= 1
	}

	t.Delete(itemKey)
	size := db.lSetMeta(metaKey, headSeq, tailSeq)
	if size == 0 {
		db.rmExpire(t, hExpType, key)
	}

	err = t.Commit()
	return value, err
}

//	ps : here just focus on deleting the list data,
//		 any other likes expire is ignore.
func (db *DB) lDelete(t *tx, key []byte) int64 {
	mk := db.lEncodeMetaKey(key)

	var headSeq int32
	var tailSeq int32
	var err error

	headSeq, tailSeq, _, err = db.lGetMeta(mk)
	if err != nil {
		return 0
	}

	var num int64 = 0
	startKey := db.lEncodeListKey(key, headSeq)
	stopKey := db.lEncodeListKey(key, tailSeq)

	it := db.db.RangeLimitIterator(startKey, stopKey, leveldb.RangeClose, 0, -1)
	for ; it.Valid(); it.Next() {
		t.Delete(it.Key())
		num++
	}
	it.Close()

	t.Delete(mk)

	return num
}

func (db *DB) lGetSeq(key []byte, whereSeq int32) (int64, error) {
	ek := db.lEncodeListKey(key, whereSeq)

	return Int64(db.db.Get(ek))
}

func (db *DB) lGetMeta(ek []byte) (headSeq int32, tailSeq int32, size int32, err error) {
	var v []byte
	v, err = db.db.Get(ek)
	if err != nil {
		return
	} else if v == nil {
		headSeq = listInitialSeq
		tailSeq = listInitialSeq
		size = 0
		return
	} else {
		headSeq = int32(binary.LittleEndian.Uint32(v[0:4]))
		tailSeq = int32(binary.LittleEndian.Uint32(v[4:8]))
		size = tailSeq - headSeq + 1
	}
	return
}

func (db *DB) lSetMeta(ek []byte, headSeq int32, tailSeq int32) int32 {
	t := db.listTx

	var size int32 = tailSeq - headSeq + 1
	if size < 0 {
		//	todo : log error + panic
	} else if size == 0 {
		t.Delete(ek)
	} else {
		buf := make([]byte, 8)

		binary.LittleEndian.PutUint32(buf[0:4], uint32(headSeq))
		binary.LittleEndian.PutUint32(buf[4:8], uint32(tailSeq))

		t.Put(ek, buf)
	}

	return size
}

func (db *DB) lExpireAt(key []byte, when int64) (int64, error) {
	t := db.listTx
	t.Lock()
	defer t.Unlock()

	if llen, err := db.LLen(key); err != nil || llen == 0 {
		return 0, err
	} else {
		db.expireAt(t, lExpType, key, when)
		if err := t.Commit(); err != nil {
			return 0, err
		}
	}
	return 1, nil
}

func (db *DB) LIndex(key []byte, index int32) ([]byte, error) {
	if err := checkKeySize(key); err != nil {
		return nil, err
	}

	var seq int32
	var headSeq int32
	var tailSeq int32
	var err error

	metaKey := db.lEncodeMetaKey(key)

	headSeq, tailSeq, _, err = db.lGetMeta(metaKey)
	if err != nil {
		return nil, err
	}

	if index >= 0 {
		seq = headSeq + index
	} else {
		seq = tailSeq + index + 1
	}

	sk := db.lEncodeListKey(key, seq)
	return db.db.Get(sk)
}

func (db *DB) LLen(key []byte) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}

	ek := db.lEncodeMetaKey(key)
	_, _, size, err := db.lGetMeta(ek)
	return int64(size), err
}

func (db *DB) LPop(key []byte) ([]byte, error) {
	return db.lpop(key, listHeadSeq)
}

func (db *DB) LPush(key []byte, args ...[]byte) (int64, error) {
	return db.lpush(key, listHeadSeq, args...)
}

func (db *DB) LRange(key []byte, start int32, stop int32) ([]interface{}, error) {
	if err := checkKeySize(key); err != nil {
		return nil, err
	}

	v := make([]interface{}, 0, 16)

	var startSeq int32
	var stopSeq int32

	if start > stop {
		return []interface{}{}, nil
	}

	var headSeq int32
	var tailSeq int32
	var err error

	metaKey := db.lEncodeMetaKey(key)

	if headSeq, tailSeq, _, err = db.lGetMeta(metaKey); err != nil {
		return nil, err
	}

	if start >= 0 && stop >= 0 {
		startSeq = headSeq + start
		stopSeq = headSeq + stop
	} else if start < 0 && stop < 0 {
		startSeq = tailSeq + start + 1
		stopSeq = tailSeq + stop + 1
	} else {
		//start < 0 && stop > 0
		startSeq = tailSeq + start + 1
		stopSeq = headSeq + stop
	}

	if startSeq < listMinSeq {
		startSeq = listMinSeq
	} else if stopSeq > listMaxSeq {
		stopSeq = listMaxSeq
	}

	startKey := db.lEncodeListKey(key, startSeq)
	stopKey := db.lEncodeListKey(key, stopSeq)
	it := db.db.RangeLimitIterator(startKey, stopKey, leveldb.RangeClose, 0, -1)
	for ; it.Valid(); it.Next() {
		v = append(v, it.Value())
	}

	it.Close()

	return v, nil
}

func (db *DB) RPop(key []byte) ([]byte, error) {
	return db.lpop(key, listTailSeq)
}

func (db *DB) RPush(key []byte, args ...[]byte) (int64, error) {
	return db.lpush(key, listTailSeq, args...)
}

func (db *DB) LClear(key []byte) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}

	t := db.listTx
	t.Lock()
	defer t.Unlock()

	num := db.lDelete(t, key)
	db.rmExpire(t, lExpType, key)

	err := t.Commit()
	return num, err
}

func (db *DB) lFlush() (drop int64, err error) {
	t := db.listTx
	t.Lock()
	defer t.Unlock()

	minKey := make([]byte, 2)
	minKey[0] = db.index
	minKey[1] = listType

	maxKey := make([]byte, 2)
	maxKey[0] = db.index
	maxKey[1] = lMetaType + 1

	it := db.db.RangeLimitIterator(minKey, maxKey, leveldb.RangeROpen, 0, -1)
	for ; it.Valid(); it.Next() {
		t.Delete(it.Key())
		drop++
		if drop&1023 == 0 {
			if err = t.Commit(); err != nil {
				return
			}
		}
	}
	it.Close()

	db.expFlush(t, lExpType)

	err = t.Commit()
	return
}

func (db *DB) LExpire(key []byte, duration int64) (int64, error) {
	if duration <= 0 {
		return 0, errExpireValue
	}

	return db.lExpireAt(key, time.Now().Unix()+duration)
}

func (db *DB) LExpireAt(key []byte, when int64) (int64, error) {
	if when <= time.Now().Unix() {
		return 0, errExpireValue
	}

	return db.lExpireAt(key, when)
}

func (db *DB) LTTL(key []byte) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return -1, err
	}

	return db.ttl(lExpType, key)
}
