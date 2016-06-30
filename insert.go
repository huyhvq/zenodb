package tdb

import (
	"fmt"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/oxtoacart/tdb/expr"
	"github.com/spaolacci/murmur3"
	"github.com/tecbot/gorocksdb"
)

type Point struct {
	Ts   time.Time
	Dims map[string]interface{}
	Vals map[string]float64
}

type partition struct {
	t            *table
	archiveDelay time.Duration
	inserts      chan *insert
	tail         map[string]*bucket
}

type insert struct {
	ts     time.Time
	t      *table
	key    []byte
	vals   map[string]expr.Value
	bucket *bucket
}

type archiveRequest struct {
	key string
	b   *bucket
}

func (db *DB) Insert(table string, point *Point) error {
	t := db.getTable(table)
	if t == nil {
		return fmt.Errorf("Unknown table %v", table)
	}

	return t.insert(point)
}

func (t *table) insert(point *Point) error {
	t.clock.Advance(point.Ts)
	vals := floatsToValues(point.Vals)
	key, err := keyToBytes(point.Dims)
	if err != nil {
		return err
	}
	h := int(murmur3.Sum32(key))
	p := h % len(t.partitions)
	select {
	case t.partitions[p].inserts <- &insert{point.Ts, t, key, vals, nil}:
		t.statsMutex.Lock()
		t.stats.InsertedPoints++
		t.statsMutex.Unlock()
	default:
		t.statsMutex.Lock()
		t.stats.DroppedPoints++
		t.statsMutex.Unlock()
	}

	return nil
}

func (p *partition) processInserts() {
	archivePeriod := p.t.archivePeriod()
	log.Debugf("Archiving every %v, delayed by %v", archivePeriod, p.archiveDelay)
	archiveTicker := p.t.clock.NewTicker(archivePeriod)
	for {
		select {
		case insert := <-p.inserts:
			p.insert(insert)
		case <-archiveTicker.C:
			p.requestArchiving()
		}
	}
}

func (p *partition) insert(insert *insert) {
	key := string(insert.key)
	now := p.t.clock.Now()
	start := roundTime(insert.ts, p.t.resolution)
	if now.Sub(start) > p.t.hotPeriod {
		log.Trace("Discarding insert outside of hot period")
		return
	}
	b := p.tail[key]
	if b == nil || b.start.Before(start) {
		p.t.statsMutex.Lock()
		if b == nil {
			p.t.stats.HotKeys++
		}
		p.t.statsMutex.Unlock()
		b = &bucket{start: start, prev: b}
		b.init(insert)
		p.tail[key] = b
		return
	}
	for {
		if b.start == start {
			// Update existing bucket
			b.update(insert)
			return
		}
		if b.prev == nil || b.prev.start.Before(start) {
			// Insert new bucket
			p.t.statsMutex.Lock()
			p.t.statsMutex.Unlock()
			b.prev = &bucket{start: start, prev: b.prev}
			b.prev.init(insert)
			return
		}
		// Continue looking
		b = b.prev
	}
}

func (insert *insert) Get(name string) expr.Value {
	// First look in fields
	val, ok := insert.vals[name]
	if ok {
		return val
	}
	fieldIdx, ok := insert.t.fieldIndexes[name]
	if ok {
		return insert.bucket.vals[fieldIdx]
	}
	return expr.Zero
}

func (p *partition) requestArchiving() {
	now := p.t.clock.Now()
	log.Debugf("Requested archiving at %v", now)
	for key, b := range p.tail {
		if now.Sub(b.start) > p.t.hotPeriod {
			log.Tracef("Archiving full. %v / %v %v", b.start, now, b.prev != nil)
			delete(p.tail, key)
			p.t.statsMutex.Lock()
			p.t.stats.HotKeys--
			p.t.statsMutex.Unlock()
			p.t.toArchive <- &archiveRequest{key, b}
			continue
		}
		next := b
		for {
			b = b.prev
			if b == nil {
				break
			}
			log.Tracef("Checking %v", b.start)
			if now.Sub(b.start) > p.t.hotPeriod {
				log.Trace("Archiving partial")
				p.t.toArchive <- &archiveRequest{key, b}
				next.prev = nil
				break
			}
		}
	}
}

func (t *table) archive() {
	batch := gorocksdb.NewWriteBatch()
	wo := gorocksdb.NewDefaultWriteOptions()
	for req := range t.toArchive {
		batch = t.doArchive(batch, wo, req)
	}
}

func (t *table) doArchive(batch *gorocksdb.WriteBatch, wo *gorocksdb.WriteOptions, req *archiveRequest) *gorocksdb.WriteBatch {
	key := []byte(req.key)
	seqs := req.b.toSequences(t.resolution)
	numPeriods := int64(seqs[0].numPeriods())
	if log.IsTraceEnabled() {
		log.Tracef("Archiving %d buckets starting at %v", numPeriods, seqs[0].start().In(time.UTC))
	}
	t.statsMutex.Lock()
	t.stats.ArchivedBuckets += numPeriods
	t.statsMutex.Unlock()
	for i, field := range t.fields {
		k, err := keyWithField(key, field.Name)
		if err != nil {
			log.Error(err)
			continue
		}
		batch.Merge(k, seqs[i])
	}
	count := int64(batch.Count())
	if count >= t.batchSize {
		err := t.archiveByKey.Write(wo, batch)
		if err != nil {
			log.Errorf("Unable to write batch: %v", err)
		}
		batch = gorocksdb.NewWriteBatch()
	}
	return batch
}

func (t *table) retain() {
	retentionTicker := t.clock.NewTicker(t.retentionPeriod)
	wo := gorocksdb.NewDefaultWriteOptions()
	for range retentionTicker.C {
		t.doRetain(wo)
	}
}

func (t *table) doRetain(wo *gorocksdb.WriteOptions) {
	log.Debug("Removing expired keys")
	start := time.Now()
	batch := gorocksdb.NewWriteBatch()
	var keysToFree []*gorocksdb.Slice
	defer func() {
		for _, k := range keysToFree {
			k.Free()
		}
	}()

	retainUntil := t.clock.Now().Add(-1 * t.retentionPeriod)
	ro := gorocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	it := t.archiveByKey.NewIterator(ro)
	defer it.Close()
	for it.SeekToFirst(); it.Valid(); it.Next() {
		v := it.Value()
		vd := v.Data()
		if vd == nil || len(vd) < size64bits || sequence(vd).start().Before(retainUntil) {
			k := it.Key()
			keysToFree = append(keysToFree, k)
			batch.Delete(k.Data())
		}
		v.Free()
	}

	if batch.Count() > 0 {
		err := t.archiveByKey.Write(wo, batch)
		if err != nil {
			log.Errorf("Unable to remove expired keys: %v", err)
		} else {
			delta := time.Now().Sub(start)
			log.Debugf("Removed %v expired keys in %v", humanize.Comma(int64(batch.Count())), delta)
		}
	} else {
		log.Debug("No expired keys to remove")
	}
}

func (t *table) archivePeriod() time.Duration {
	return t.hotPeriod / 10
}

func floatsToValues(in map[string]float64) map[string]expr.Value {
	out := make(map[string]expr.Value, len(in))
	for key, value := range in {
		out[key] = expr.Float(value)
	}
	return out
}