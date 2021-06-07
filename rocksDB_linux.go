package ytfs

import (
	"encoding/binary"
	"fmt"
	"github.com/mr-tron/base58/base58"
	"github.com/tecbot/gorocksdb"
	ydcommon "github.com/yottachain/YTFS/common"
	"github.com/yottachain/YTFS/opt"
	"os"
	"path"
	"sort"
	"sync"
	"unsafe"
)

var mdbFileName = "/maindb"
var ytPosKey    = "yt_rocks_pos_key"
var ytBlkSzKey  = "yt_blk_size_key"
var VerifyedKvFile string = "/gc/rock_verify"
//var hash0Str = "0000000000000000"

type KvDB struct {
	Rdb *gorocksdb.DB
	ro  *gorocksdb.ReadOptions
	wo  *gorocksdb.WriteOptions
	PosKey ydcommon.IndexTableKey
	PosIdx ydcommon.IndexTableValue
	BlkKey ydcommon.IndexTableKey
	BlkVal uint32
	Header *ydcommon.Header
}

//type Hashtohash struct {
//	DBhash []byte
//	Datahash []byte
//}

func openKVDB(DBPath string) (kvdb *KvDB, err error) {
//	var posIdx uint32
	bbto := gorocksdb.NewDefaultBlockBasedTableOptions()
	bbto.SetBlockCache(gorocksdb.NewLRUCache(3 << 30))
	opts := gorocksdb.NewDefaultOptions()
	opts.SetBlockBasedTableFactory(bbto)
	opts.SetCreateIfMissing(true)

	db, err := gorocksdb.OpenDb(opts, DBPath)
	if err != nil {
		fmt.Println("[kvdb] open rocksdb error")
		return nil, err
	}

	ro := gorocksdb.NewDefaultReadOptions()
	wo := gorocksdb.NewDefaultWriteOptions()

	return &KvDB{
		Rdb   :  db,
		ro    :  ro,
		wo    :  wo,
		//PosKey:  ydcommon.IndexTableKey(diskPoskey),
		//PosIdx:  ydcommon.IndexTableValue(posIdx),
	}, err
}

func openYTFSK(dir string, config *opt.Options) (*YTFS, error) {
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
	mainDBPath := path.Join(dir, mdbFileName)
	mDB, err := openKVDB(mainDBPath)
	if err != nil {
		fmt.Println("[KVDB]open main kv-DB for save hash error:", err)
		return nil, err
	}

	Header,err := initializeHeader(config)
	if err != nil {
		fmt.Println("[rocksdb]initialize Header error")
		return nil,err
	}
    mDB.Header = Header

	//get start Pos from rocksdb
	HKey := ydcommon.BytesToHash([]byte(ytPosKey))
	mDB.PosKey = ydcommon.IndexTableKey(HKey)
	PosRocksdb, err := mDB.Get(mDB.PosKey)
	if err != nil {
		fmt.Println("[rocksdb] get start write pos err=",err)
		return nil, err
	}

	//if indexdb exist, get write start pos from index.db
	fileIdxdb := path.Join(dir,"index.db")
	if PathExists(fileIdxdb){
		indexDB, err := NewIndexDB(dir, config)
		if err != nil {
			return nil,err
		}

		//if rocksdb start pos < index.db start pos, there must be some error
		posIdxdb := indexDB.schema.DataEndPoint
		if uint64(PosRocksdb) < posIdxdb{
			fmt.Println("pos error:",ErrDBConfig)
			return nil,ErrDBConfig
		}
	}

	mDB.PosIdx = PosRocksdb
	fmt.Println("[rocksdb] OpenYTFSK Current start posidx=",mDB.PosIdx)

	//check blksize to rocksdb
	HKey = ydcommon.BytesToHash([]byte(ytBlkSzKey))
	mDB.BlkKey = ydcommon.IndexTableKey(HKey)
	Blksize,err := mDB.Get(mDB.BlkKey)
	if  err != nil  {
		fmt.Println("[rocksdb] get BlkSize error")
		return nil,err
	}

	valbuf := make([]byte,4)
	if uint32(Blksize) != Header.DataBlockSize {
		if uint32(Blksize) != 0{
			fmt.Println("[rocksdb] error, BlkSize mismatch")
			return nil,err
		}

		binary.LittleEndian.PutUint32(valbuf, uint32(Header.DataBlockSize))
		err := mDB.Rdb.Put(mDB.wo, mDB.BlkKey[:], valbuf)
		if err != nil {
			fmt.Println("[rocksdb]set blksize to rocksdb err:", err)
			return nil, err
		}
	}

	//3. open storages
	context, err := NewContext(dir, config, uint64(mDB.PosIdx))
	if err != nil {
		return nil, err
	}

	ytfs := &YTFS{
		config : config,
		db     : mDB,
		context: context,
		mutex  : new(sync.Mutex),
	}

	fileName := path.Join(dir, "dbsafe")
	if ! PathExists(fileName) {
		if _, err := os.Create(fileName);err != nil {
			fmt.Println("create arbiration file error!")
			return nil,err
		}
	}

	fmt.Println("Open YTFS success @" + dir)
	return ytfs, nil
}


func (rd *KvDB) Get(key ydcommon.IndexTableKey) (ydcommon.IndexTableValue, error) {
	var retval uint32
	val, err := rd.Rdb.Get(rd.ro, key[:])
	if err != nil {
		fmt.Println("[rocksdb] get pos error:", err)
		return 0, err
	}

	if val.Exists(){
		retval = binary.LittleEndian.Uint32(val.Data())
	}
	return ydcommon.IndexTableValue(retval), nil
}

func initializeHeader( config *opt.Options) (*ydcommon.Header, error) {
	m, n := config.IndexTableCols, config.IndexTableRows
	t, d, h := config.TotalVolumn, config.DataBlockSize, uint32(unsafe.Sizeof(ydcommon.Header{}))

	ytfsSize := uint64(0)
	for _, storageOption := range config.Storages {
		ytfsSize += storageOption.StorageVolume
	}

	// write header.
	header := ydcommon.Header{
		Tag:            [4]byte{'Y', 'T', 'F', 'S'},
		Version:        [4]byte{'0', '.', '0', '3'},
		YtfsCapability: t,
		YtfsSize:       ytfsSize,
		DataBlockSize:  d,
		RangeCapacity:  n,
		RangeCoverage:  m,
		HashOffset:     h,
		DataEndPoint:   0,
		RecycleOffset:  uint64(h) + (uint64(n)+1)*(uint64(m)*36+4),
		Reserved:       0xCDCDCDCDCDCDCDCD,
	}
	return &header, nil
}

func (rd *KvDB) UpdateMeta(account uint64) error {
	buf := make([]byte, 4)
	fmt.Println("[rockspos] BatchPut PosIdx=",rd.PosIdx,"account=",account)
	rd.PosIdx = ydcommon.IndexTableValue(uint32(rd.PosIdx) + uint32(account))
	binary.LittleEndian.PutUint32(buf, uint32(rd.PosIdx))
	err := rd.Rdb.Put(rd.wo,rd.PosKey[:],buf)
	if err != nil {
		fmt.Println("update write pos to db error:", err)
	}
	return  err
}

func (rd *KvDB) BatchPut(kvPairs []ydcommon.IndexItem) (map[ydcommon.IndexTableKey]byte, error) {
	//	keyValue:=make(map[ydcommon.IndexTableKey]ydcommon.IndexTableValue,len(batch))
	valbuf := make([]byte, 4)
	for _,value := range kvPairs{
		HKey := value.Hash[:]
		HPos := value.OffsetIdx
		binary.LittleEndian.PutUint32(valbuf, uint32(HPos))
		err := rd.Rdb.Put(rd.wo, HKey, valbuf)

		if err != nil {
			fmt.Println("[rocksdb]put dnhash to rocksdb error:", err)
			return nil, err
		}
	}

	//fmt.Printf("[noconflict] write success batch_write_time: %d ms, batch_len %d", time.Now().Sub(begin).Milliseconds(), bufCnt)
	return nil, nil
}

func (rd *KvDB) BatchWriteKV(batch map[ydcommon.IndexTableKey][]byte) error {
	var err error
	Wbatch := new(gorocksdb.WriteBatch)
	for key, val := range batch {
		Wbatch.Put(key[:], val)

	}
	err = rd.Rdb.Write(rd.wo, Wbatch)
	return err
}

func (rd *KvDB) resetKV(batchIndexes []ydcommon.IndexItem, resetCnt uint32) {
	for j := uint32(0); j < resetCnt; j++ {
		hashKey := batchIndexes[j].Hash[:]
		rd.Rdb.Delete(rd.wo, hashKey[:])
	}
}

func (rd *KvDB) Len() uint64 {
	gcspace,err := rd.Rdb.Get(rd.ro,[]byte(gcspacecntkey))
	if err == nil && gcspace.Data() !=nil {
		val := binary.LittleEndian.Uint32(gcspace.Data())
		return uint64(rd.PosIdx) - uint64(val)
	}
	return uint64(rd.PosIdx)
}

func (rd *KvDB) TotalSize() uint64{
	return rd.Header.YtfsSize
}

func (rd *KvDB) BlockSize() uint32{
	return rd.BlkVal
}

func (rd *KvDB) Meta() *ydcommon.Header{
	return rd.Header
}

func (rd *KvDB) Put(key ydcommon.IndexTableKey, value ydcommon.IndexTableValue) error {
	valbuf := make([]byte,4)
	binary.LittleEndian.PutUint32(valbuf, uint32(value))
	return rd.Rdb.Put(rd.wo, key[:], valbuf)
	//return nil
}

func (rd *KvDB) Delete(key ydcommon.IndexTableKey) error {
	return rd.Rdb.Delete(rd.wo, key[:])
}

func (rd *KvDB) PutDb(key, value []byte) error {
	return rd.Rdb.Put(rd.wo,key,value)
}

func (rd *KvDB) GetDb(key []byte) ([]byte, error) {
	 slice,err:=rd.Rdb.Get(rd.ro, key)
	 if err != nil {
	 	return nil, err
	 }
	 data := slice.Data()
	 return data, nil
}

func (rd *KvDB) DeleteDb(key []byte) error {
	return rd.Rdb.Delete(rd.wo, key)
}

func (rd *KvDB)GetBitMapTab(num int) ([]ydcommon.GcTableItem,error){
	var gctab []ydcommon.GcTableItem
	var n int

	ro := gorocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	iter := rd.Rdb.NewIterator(ro)
	prefix := []byte("del")

	for iter.Seek(prefix);iter.ValidForPrefix(prefix);iter.Next(){
		key := iter.Key().Data()
		fmt.Println("[gcdel] kvdb-GetBitMapTab,key=",string(key[0:3])+base58.Encode(key[3:]),"len(key)=",len(key))
		if len(iter.Key().Data()) != ydcommon.GcHashLen{
			continue
		}

		if len(iter.Value().Data()) == 0{
			continue
		}

		var gctabItem ydcommon.GcTableItem
		copy(gctabItem.Gckey[:],iter.Key().Data())
		gctabItem.Gcval = ydcommon.GcTableValue(binary.LittleEndian.Uint32(iter.Value().Data()))
		gctab = append(gctab,gctabItem)
		n++
		if n >= num{
			break
		}
	}
	fmt.Println("[gcdel] kvdb-GetBitMapTab, len(gctab)=",len(gctab))
    return gctab,nil
}

func (rd *KvDB) Close() {
}

func (rd *KvDB) Reset() {
}

func (rd *KvDB) TravelDB(fn func(key, value []byte) error) int64 {
	ro := gorocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	iter := rd.Rdb.NewIterator(ro)
	succ := 0
	for iter.SeekToFirst(); iter.Valid(); iter.Next(){
		if err := fn(iter.Key().Data(),iter.Value().Data()); err != nil{
			fmt.Println("[travelDB] exec fn() err=",err,"key=",iter.Key().Data(),"value=",iter.Value().Data())
			continue
		}
		succ++
	}
	return int64(succ)
}

func (rd *KvDB)GetSettedIter(startkey string) *gorocksdb.Iterator{
	fmt.Println("startkey=",startkey)
	ro := gorocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	iter := rd.Rdb.NewIterator(ro)
	if len(startkey)==0 || startkey == "0"{
		iter.SeekToFirst()
	}else{
		begin,_ := base58.Decode(startkey)
		iter.Seek(begin)
	}
	return iter
}

func (rd *KvDB) TravelDBforverify(fn func(key ydcommon.IndexTableKey) (Hashtohash,error), startkey string, traveEntries uint64) ([]Hashtohash, string, error) {
	var hashTab []Hashtohash
	//var hashKey ydcommon.IndexTableKey
	//var pos ydcommon.IndexTableValue
	var err error
	var beginKey string
	var verifyTab []ydcommon.IndexItem
	nilStr =""

	iter := rd.GetSettedIter(startkey)
	num := uint64(0)
	for ; iter.Valid(); iter.Next() {
		num++
		if num > traveEntries {
			break
		}
		var verifyItem ydcommon.IndexItem
		copy(verifyItem.Hash[:], iter.Key().Data())
		verifyItem.OffsetIdx = ydcommon.IndexTableValue(binary.LittleEndian.Uint32(iter.Value().Data()))
		verifyTab = append(verifyTab, verifyItem)
	}

	if verifyTab == nil||len(verifyTab)==0{
		fmt.Println("[verify][error] verifyTab is nil")
		return nil, nilStr, nil
	}

	sort.Slice(verifyTab, func(i, j int) bool {
		return verifyTab[i].OffsetIdx < verifyTab[j].OffsetIdx
	})

	for _ , v := range verifyTab{
			ret,err := fn(v.Hash)
			if err != nil{
				fmt.Println("[verify][travelDB] verify error:",err,"key=",base58.Encode(iter.Key().Data()))
				hashTab = append(hashTab,ret)
		        continue
			}
			fmt.Println("[verify][travelDB] verify succ,key=",base58.Encode(iter.Key().Data()),"value=",iter.Value().Data(),"num=",num)
	}

	if !iter.Valid(){
		beginKey = "0"
	}else{
		beginKey = base58.Encode(iter.Key().Data())
	}
	return hashTab,beginKey,err
}

func (rd *KvDB) ScanDB(){

}