package ytfs

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	//	"github.com/syndtr/goleveldb/leveldb"
	"github.com/tecbot/gorocksdb"
	//	"github.com/linux-go/go1.13.5.linux-amd64/go/src/time"
	"github.com/mr-tron/base58/base58"
	"github.com/yottachain/YTDataNode/util"
	ydcommon "github.com/yottachain/YTFS/common"
	"github.com/yottachain/YTFS/opt"
	_ "net/http/pprof"
	"os"
	"path"
	"sync"
	//	log "github.com/yottachain/YTDataNode/logger"
)

var  mdbFileName = "/maindb"

type ytfsStatus struct {
	ctxSP *storagePointer
	//TODO: index status
}

type KvDB struct{
	Rdb *gorocksdb.DB
	ro  *gorocksdb.ReadOptions
	wo  *gorocksdb.WriteOptions
}

// YTFS is a data block save/load lib based on key-value styled db APIs.
type YTFS struct {
	// config of this YTFS
	config *opt.Options
	// key-value db which saves hash <-> position
	db *IndexDB
	// main rocksdb
	mdb *KvDB                      //todo xiaojm
	// running context
	context *Context
	// lock of YTFS
	mutex *sync.Mutex
	// saved status
	savedStatus []ytfsStatus
}

// Open opens or creates a YTFS for the given storage.
// The YTFS will be created if not exist.
//
// The returned YTFS instance is safe for concurrent use.
// The YTFS must be closed after use, by calling Close method.
// Usage Sample, ref to playground.go:
//		...
//		config := opt.DefaultOptions()
//
//		ytfs, err := ytfs.Open(path, config)
//		if err != nil {
//			panic(err)
//		}
//		defer ytfs.Close()
//		err = ytfs.Put(ydcommon.IndexTableKey, ydcommon.IndexTableValue)
//		if err != nil {
//			panic(err)
//		}
//
//		ydcommon.IndexTableValue, err = ytfs.Gut(ydcommon.IndexTableKey)
//		if err != nil {
//			panic(err)
//		}
//		...
func Open(dir string, config *opt.Options) (ytfs *YTFS, err error) {
	settings, err := opt.FinalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return openYTFS(dir, settings)
}

// NewYTFS create a YTFS by config
func NewYTFS(dir string, config *opt.Options) (*YTFS, error) {
	ytfs := new(YTFS)
	indexDB, err := NewIndexDB(dir, config)
	if err != nil {
		return nil, err
	}
	context, err := NewContext(dir, config, indexDB.schema.DataEndPoint)
	if err != nil {
		return nil, err
	}
	ytfs.db = indexDB
	ytfs.context = context
	ytfs.mutex = new(sync.Mutex)
	return ytfs, nil
}

//func openKVDB(DBPath string) (db *leveldb.DB,err error){
//	db,err = leveldb.OpenFile(DBPath,nil)
//	if err != nil{
//		fmt.Printf("open DB:%s error",DBPath)
//		return nil,err
//	}
//	return db,err
//}

func openKVDB(DBPath string) (kvdb *KvDB,err error){
	// 使用 gorocksdb 连接 RocksDB
	bbto := gorocksdb.NewDefaultBlockBasedTableOptions()
	bbto.SetBlockCache(gorocksdb.NewLRUCache(3 << 30))
	opts := gorocksdb.NewDefaultOptions()
	opts.SetBlockBasedTableFactory(bbto)
	opts.SetCreateIfMissing(true)
	// 设置输入目标数据库文件（可自行配置，./db 为当前测试文件的目录下的 db 文件夹）
	db, err := gorocksdb.OpenDb(opts, DBPath)
	if err != nil {
		fmt.Println("[kvdb] open rocksdb error")
		return nil,err
	}

	// 创建输入输出流
	ro := gorocksdb.NewDefaultReadOptions()
	wo := gorocksdb.NewDefaultWriteOptions()
	return &KvDB {
		Rdb: db,
		ro:  ro,
		wo:  wo,
	},err
}

func openYTFS(dir string, config *opt.Options) (*YTFS, error) {
	//TODO: file lock to avoid re-open.
	//1. open system dir for YTFS
	if fi, err := os.Stat(dir); err == nil {
		// dir/file exists, check if it can be reloaded.
		if !fi.IsDir() {
			return nil, ErrDirNameConflict
		}
		err := openYTFSDir(dir, config)
		if err != nil && err != ErrEmptyYTFSDir {
			return nil, err
		}
	} else {
		// create new dir
		if err := os.MkdirAll(dir, os.ModeDir|os.ModePerm); err != nil {
			return nil, err
		}
	}

	// initial a new ytfs.
	// save config
	configName := path.Join(dir, "config.json")
	err := opt.SaveConfig(config, configName)
	if err != nil {
		return nil, err
	}

	//open main kv-db
	mainDBPath := path.Join(dir,mdbFileName)
	mDB,err := openKVDB(mainDBPath)
	if err != nil {
		fmt.Println("[KVDB]open main kv-DB for save hash error:",err)
		return nil,err
	}

	// open index db
	indexDB, err := NewIndexDB(dir, config)
	if err != nil {
		return nil, err
	}

	//3. open storages
	context, err := NewContext(dir, config, indexDB.schema.DataEndPoint)
	if err != nil {
		return nil, err
	}

	ytfs := &YTFS{
		config:  config,
		mdb:     mDB,
		db:      indexDB,
		context: context,
		mutex:   new(sync.Mutex),
	}
    ytfs.config.UseKvDb = true
	fmt.Println("Open YTFS success @" + dir)
	return ytfs, nil
}

func openYTFSDir(dir string, config *opt.Options) error {
	configPath := path.Join(dir, "config.json")
	if _, err := os.Stat(configPath); err == nil {
		// TODO: recover data and check config consistency with input.
		oldConfig, err := opt.ParseConfig(configPath)
		if err != nil {
			return err
		}

		if !oldConfig.Equal(config) {
			return ErrSettingMismatch
		}

		return nil
	}

	return ErrEmptyYTFSDir
}

func validateYTFSSchema(meta *ydcommon.Header, opt *opt.Options) (*ydcommon.Header, *opt.Options, error) {
	if meta.YtfsCapability != opt.TotalVolumn || meta.DataBlockSize != opt.DataBlockSize {
		return nil, nil, ErrConfigIndexMismatch
	}
	return meta, opt, nil
}

// Get gets the value for the given key. It returns ErrNotFound if the
// DB does not contains the key.
//
// The returned slice is its own copy, it is safe to modify the contents
// of the returned slice.
// It is safe to modify the contents of the argument after Get returns.
func (ytfs *YTFS) Get(key ydcommon.IndexTableKey) ([]byte, error) {
	if ytfs.config.UseKvDb{
		fmt.Println("[rocksdb] use rocksdb for matadata")
		return ytfs.GetK(key)
	}
	return ytfs.GetI(key)
}

func (ytfs *YTFS) GetI(key ydcommon.IndexTableKey) ([]byte, error) {
	pos, err := ytfs.db.Get(key)
	if err != nil {
		fmt.Println("[indexdb] indexdb get pos error:",err)
		return nil, err
	}
	return ytfs.context.Get(pos)
}

func (ytfs *YTFS) GetK(key ydcommon.IndexTableKey) ([]byte, error) {
	val, err := ytfs.mdb.Rdb.Get(ytfs.mdb.ro,key[:])
	pos := binary.LittleEndian.Uint32(val.Data())
//	fmt.Println("[rocksdb] Rocksdbval=",val,"Rocksdbval32=",pos)
	if err != nil {
		fmt.Println("[rocksdb] rocksdb get pos error:",err)
		return nil, err
	}

	return ytfs.context.Get(ydcommon.IndexTableValue(pos))
}

// Put sets the value for the given key. It panic if there exists any previous value
// for that key; YottaDisk is not a multi-map.
// It is safe to modify the contents of the arguments after Put returns but not
// before.
func (ytfs *YTFS) Put(key ydcommon.IndexTableKey, buf []byte) error {
	ytfs.mutex.Lock()
	defer ytfs.mutex.Unlock()
	_, err := ytfs.db.Get(key)
	if err == nil {
		return ErrDataConflict
	}

	pos, err := ytfs.context.Put(buf)
	if err != nil {
		return err
	}

	return ytfs.db.Put(key, ydcommon.IndexTableValue(pos))
}

/*
 * Batch mode func list
 */
func (ytfs *YTFS) restoreYTFS() {
	//TODO: save index
	fmt.Println("[rocksdb] in restoreYTFS()")
	id := len(ytfs.savedStatus) - 1
	ydcommon.YottaAssert(id >= 0)
	ytfs.context.restore(ytfs.savedStatus[id].ctxSP)
	ytfs.savedStatus = ytfs.savedStatus[:id]
}

func (ytfs *YTFS) restoreIndex(conflict map[ydcommon.IndexTableKey]byte, batchindex []ydcommon.IndexItem, btCnt uint32) error {
    var err error
    tbItemMap := make(map[uint32]uint32,btCnt)
	for _, kvPairs := range batchindex {
    	hashkey := kvPairs.Hash
    	if _, ok := conflict[hashkey]; ok{
    		fmt.Println("[restoreIndex] hashkey conflict:",base58.Encode(hashkey[:]))
    		continue
		}
    	idx := ytfs.db.indexFile.GetTableEntryIndex(hashkey)
		err = ytfs.db.indexFile.ClearItemFromTable(idx, hashkey,btCnt,tbItemMap)
		if err != nil {
			fmt.Printf("[restoreIndex] reset tableidx %v hashkey %v \n",idx,hashkey)
			return err
		}
	}
	err = ytfs.db.indexFile.ResetTableSize(tbItemMap)
	if err != nil {
		fmt.Println("[restoreIndex] ResetTableSize error")
	}
	return err
}

func (ytfs *YTFS) saveCurrentYTFS() {
	//TODO: restore index
	ytfs.savedStatus = append(ytfs.savedStatus, ytfsStatus{
		ctxSP: ytfs.context.save(),
	})
}

func (ytfs *YTFS)checkConflicts(conflicts map[ydcommon.IndexTableKey]byte, batch map[ydcommon.IndexTableKey][]byte){
	dir := util.GetYTFSPath()
	fileName := path.Join( dir, "hashconflict.new")
	hashConflict,_ := os.OpenFile(fileName, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	defer hashConflict.Close()

	fileName2 := path.Join( dir, "hashconflict.old")
	hashConflict2,_ := os.OpenFile(fileName2, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	defer hashConflict2.Close()

	for ha, cflct := range conflicts {
        if cflct == 1 {
        	hashConflict.WriteString("hash:")
        	hashConflict.WriteString(base58.Encode(ha[:]))
			hashConflict.WriteString("\n\r")
        	hashConflict.Write(batch[ha])

			hashConflict2.WriteString("hash:")
			hashConflict2.WriteString(base58.Encode(ha[:]))
			hashConflict2.WriteString("\n\r")
        	oldData,err := ytfs.Get(ha)
        	if err != nil {
        		fmt.Printf("get hash conflict slice data err,hash:%v",base58.Encode(ha[:]))
			}

			hashConflict2.Write(oldData)
			fmt.Printf("find hash conflict, hash:%v",base58.Encode(ha[:]))
		}
	}
}

func (ytfs *YTFS) BatchWriteKV(batch map[ydcommon.IndexTableKey][]byte) error {
	var err error
	Wbatch := new(gorocksdb.WriteBatch)
	for key,val := range batch {
		Wbatch.Put(key[:],val)

	}
	err = ytfs.mdb.Rdb.Write(ytfs.mdb.wo, Wbatch)
	return err
}

func (ytfs *YTFS)resetKV(batchIndexes []ydcommon.IndexItem,resetCnt uint32){
	for j:= uint32(0); j < resetCnt; j++ {
		hashKey := batchIndexes[j].Hash[:]
		ytfs.mdb.Rdb.Delete(ytfs.mdb.wo,hashKey[:])
	}
}

//var mutexindex uint64 = 0
// BatchPut sets the value array for the given key array.
// It panics if there exists any previous value for that key as YottaDisk is not a multi-map.
// It is safe to modify the contents of the arguments after Put returns but not
// before.
func (ytfs *YTFS) BatchPut(batch map[ydcommon.IndexTableKey][]byte) (map[ydcommon.IndexTableKey]byte, error) {
	if ytfs.config.UseKvDb {
		fmt.Println("[rocksdb] write use rocksdb for matadata")
		return ytfs.BatchPutK(batch)
	}
	return ytfs.BatchPutI(batch)
}

func (ytfs *YTFS) BatchPutI(batch map[ydcommon.IndexTableKey][]byte) (map[ydcommon.IndexTableKey]byte, error) {
		ytfs.mutex.Lock()
		defer ytfs.mutex.Unlock()

		if len(batch) > 1000 {
			return nil, fmt.Errorf("Batch Size is too big")
		}
		fmt.Println("[indexdb] BatchPutI len(batch)=",len(batch))

		// NO get check, but retore all status if error
		ytfs.saveCurrentYTFS()
		batchIndexes := make([]ydcommon.IndexItem, len(batch))
		batchBuffer := []byte{}
		bufCnt := len(batch)
		i := 0
		for k, v := range batch {
			batchBuffer = append(batchBuffer, v...)
			batchIndexes[i] = ydcommon.IndexItem{
				Hash:      k,
				OffsetIdx: ydcommon.IndexTableValue(0)}
			i++
		}

		startPos, err := ytfs.context.BatchPut(bufCnt, batchBuffer)

		if err != nil {
			fmt.Println("[indexdb] ytfs.context.BatchPut error")
			ytfs.restoreYTFS()
			return nil, err
		}

		for i := uint32(0); i < uint32(bufCnt); i++ {
			batchIndexes[i] = ydcommon.IndexItem{
				Hash:      batchIndexes[i].Hash,
				OffsetIdx: ydcommon.IndexTableValue(startPos + i)}

		}

		conflicts, err := ytfs.db.BatchPut(batchIndexes)

		if err != nil {
			fmt.Println("[indexdb]  update indexdb error:",err)
			ytfs.restoreIndex(conflicts, batchIndexes, uint32(bufCnt))
			ytfs.restoreYTFS()
			return conflicts, err
		}

		return nil, nil
}

func (ytfs *YTFS) BatchPutK(batch map[ydcommon.IndexTableKey][]byte) (map[ydcommon.IndexTableKey]byte, error) {
	ytfs.mutex.Lock()
	defer ytfs.mutex.Unlock()

	if len(batch) > 1000 {
		return nil, fmt.Errorf("Batch Size is too big")
	}
	// NO get check, but retore all status if error
	ytfs.saveCurrentYTFS()

	batchIndexes := make([]ydcommon.IndexItem, len(batch))
	batchBuffer := []byte{}
	bufCnt := len(batch)
	i := 0
	for k, v := range batch {
		batchBuffer = append(batchBuffer, v...)
		batchIndexes[i] = ydcommon.IndexItem{
			Hash:      k,
			OffsetIdx: ydcommon.IndexTableValue(0)}
		i++
	}

	startPos, err := ytfs.context.BatchPut(bufCnt, batchBuffer)

	if err != nil {
		fmt.Println("[rocksdb] ytfs.context.BatchPut error")
		ytfs.restoreYTFS()
		return nil, err
	}

//	keyValue:=make(map[ydcommon.IndexTableKey]ydcommon.IndexTableValue,len(batch))
	valbuf := make([]byte,4)
	for i := uint32(0); i < uint32(bufCnt); i++ {
		HKey := batchIndexes[i].Hash[:]
		binary.LittleEndian.PutUint32(valbuf, uint32(startPos + i))
		err = ytfs.mdb.Rdb.Put(ytfs.mdb.wo, HKey, valbuf)

		if err !=nil {
			fmt.Println("[rocksdb]put dnhash to rocksdb error",err)
			ytfs.resetKV(batchIndexes,i)
			ytfs.restoreYTFS()
			return nil,err
		}
	}

	return nil, nil
}

// Meta reports current meta information.
func (ytfs *YTFS) Meta() *ydcommon.Header {
	return ytfs.db.schema
}

// Close closes the YTFS.
//
// It is valid to call Close multiple times. Other methods should not be
// called after the DB has been closed.
func (ytfs *YTFS) Close() {
	ytfs.db.Close()
	ytfs.context.Close()
}

// Reset resets an existed YottaDisk, and make it ready
// for next put/get operation. so far we do quick format which just
// erases the header.
func (ytfs *YTFS) Reset() error {
	ytfs.db.Reset()
	ytfs.context.Reset()
	return nil
}

// Cap report capacity of YTFS, just like cap() of a slice
func (ytfs *YTFS) Cap() uint64 {
	cap := uint64(0)
	for _, stroageCtx := range ytfs.context.storages {
		cap += uint64(stroageCtx.Cap)
	}
	return cap
}

// Len report len of YTFS, just like len() of a slice
func (ytfs *YTFS) Len() uint64 {
	return ytfs.db.schema.DataEndPoint
}

// String reports current YTFS status.
func (ytfs *YTFS) String() string {
	meta, _ := json.MarshalIndent(ytfs.db.schema, "", "	")
	// min := (int64)(math.MaxInt64)
	// max := (int64)(math.MinInt64)
	// sum := (int64)(0)
	// table := fmt.Sprintf("Total table Count: %d\n"+
	// 	"Total saved items: %d\n"+
	// 	"Maximum table size: %d\n"+
	// 	"Minimum table size: %d\n"+
	// 	"Average table size: %d\n", len(disk.index.sizes), sum, max, min, avg)
	return string(meta) + "\n"
}
