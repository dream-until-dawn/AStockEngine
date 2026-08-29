// Package fingerprint 实现 C5 可复现性的可验证形式。
//
//	输入指纹 = sha256( 规范化配置 ‖ 数据指纹 ‖ 引擎版本 )
//	输出指纹 = sha256( 逐笔成交 ‖ 最终账本 )
//
// **同输入指纹必须给出同输出指纹。** 这是「同配置两次运行逐笔一致」
// 这句话能被机器检验的写法 —— 光靠肉眼比对几个汇总数，
// 中间某一笔成交价错了看不出来。
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ---- 引擎版本 ----

// engineVersion 由 main 在启动时注入（-ldflags -X main.gitCommit=...）。
var (
	mu            sync.RWMutex
	engineVersion = "dev"
)

// SetEngineVersion 设置引擎版本。空值与 "dev" 等价。
func SetEngineVersion(v string) {
	mu.Lock()
	defer mu.Unlock()
	if v == "" {
		v = "dev"
	}
	engineVersion = v
}

// EngineVersion 返回引擎版本。
func EngineVersion() string {
	mu.RLock()
	defer mu.RUnlock()
	return engineVersion
}

// Reproducible 报告本次构建的指纹是否可跨构建复现。
//
// `go run` 拿不到 git commit，版本退化为 "dev"。**此时指纹相同并不代表
// 结果可复现** —— 两次 dev 构建之间源码可能已经变了。报告里必须标出来，
// 否则指纹就是在撒谎。
func Reproducible() bool { return EngineVersion() != "dev" }

// ---- 通用 ----

// Hex 返回若干段字节的 sha256 十六进制串。
func Hex(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		// 每段前缀长度，避免 ("ab","c") 与 ("a","bc") 撞出同一个摘要
		fmt.Fprintf(h, "%d:", len(p))
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Short 截取指纹前 12 位，供人眼比对。**只用于显示** ——
// 判定是否一致必须用全长。
func Short(fp string) string {
	if len(fp) <= 12 {
		return fp
	}
	return fp[:12]
}

// ---- 数据指纹 ----

// fileEntry 是缓存里的一条。
type fileEntry struct {
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	Hash  string `json:"hash"`
}

type cacheFile struct {
	Version int                  `json:"version"`
	Files   map[string]fileEntry `json:"files"`
}

const cacheName = ".fingerprint.json"

// Data 计算数据目录的内容指纹。
//
// **哈希的是内容，不是 mtime。** 用 mtime 的话，把 data/ 复制一份指纹就变了 ——
// 而那明明是同一份数据。缓存用 (相对路径, 大小, mtime) 作键只是为了跳过重算，
// 缓存失效时重算出的结果与原来一致。
//
// 只覆盖 parquet：CSV 镜像是派生物，_sync_state.json 是 ETL 的进度记录，
// 两者都不影响回测结果。
func Data(root string) (fp string, files int, err error) {
	paths, err := parquetFiles(root)
	if err != nil {
		return "", 0, err
	}
	if len(paths) == 0 {
		return "", 0, fmt.Errorf("%s 下没有 parquet 文件", root)
	}

	cache := loadCache(root)
	dirty := false
	h := sha256.New()

	for _, rel := range paths {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		st, err := os.Stat(abs)
		if err != nil {
			return "", 0, err
		}
		e, ok := cache.Files[rel]
		if !ok || e.Size != st.Size() || e.MTime != st.ModTime().UnixNano() {
			sum, err := hashFile(abs)
			if err != nil {
				return "", 0, err
			}
			e = fileEntry{Size: st.Size(), MTime: st.ModTime().UnixNano(), Hash: sum}
			cache.Files[rel] = e
			dirty = true
		}
		// 相对路径进哈希、绝对路径不进 —— 换个目录放不该改变指纹
		fmt.Fprintf(h, "%s\x00%d\x00%s\n", rel, e.Size, e.Hash)
	}
	if dirty {
		saveCache(root, cache) // 写失败不影响正确性，只是下次要重算
	}
	return hex.EncodeToString(h.Sum(nil)), len(paths), nil
}

// parquetFiles 列出数据目录下的全部 parquet，返回**排序后的相对路径**（斜杠分隔）。
//
// 排序是必须的：目录遍历顺序在不同文件系统上不同，
// 不排序会让同一份数据在两台机器上算出不同指纹。
func parquetFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// 缓存目录与备份目录不参与
			switch d.Name() {
			case "cache", "_backup", "results", "snapshots":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), "_bench") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".parquet") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func loadCache(root string) cacheFile {
	c := cacheFile{Version: 1, Files: map[string]fileEntry{}}
	b, err := os.ReadFile(filepath.Join(root, cacheName))
	if err != nil {
		return c
	}
	var got cacheFile
	if json.Unmarshal(b, &got) != nil || got.Version != 1 || got.Files == nil {
		return c
	}
	return got
}

func saveCache(root string, c cacheFile) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(root, cacheName), b, 0o644)
}
