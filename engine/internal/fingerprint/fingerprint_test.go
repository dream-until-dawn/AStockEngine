package fingerprint

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeData 造一个最小的数据目录：两个 parquet（内容是假的，只哈希字节）
// 外加若干不该参与指纹的东西。
func fakeData(t *testing.T, dir string) {
	t.Helper()
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("meta/instruments.parquet", "INSTRUMENTS")
	write("bar/market=ashare/freq=1d/year=2024/part-00000.parquet", "BARS-2024")
	write("bar/market=ashare/freq=1d/year=2025/part-00000.parquet", "BARS-2025")
	// 以下都不该进指纹
	write("meta/instruments.csv", "派生物")
	write("meta/_sync_state.json", "ETL 进度")
	write("cache/whatever.parquet", "缓存")
	write("_backup/old.parquet", "备份")
	write("results/run.csv", "结果")
}

func fp(t *testing.T, dir string) (string, int) {
	t.Helper()
	f, n, err := Data(dir)
	if err != nil {
		t.Fatal(err)
	}
	return f, n
}

// TestDataFingerprintStable 同一份数据两次算出同一个指纹。
func TestDataFingerprintStable(t *testing.T) {
	dir := t.TempDir()
	fakeData(t, dir)
	a, n := fp(t, dir)
	b, _ := fp(t, dir)
	if a != b {
		t.Fatalf("同一份数据算出两个指纹：\n  %s\n  %s", a, b)
	}
	if n != 3 {
		t.Errorf("应当只数 3 个 parquet（cache / _backup 要排除），得到 %d", n)
	}
}

// TestDataFingerprintSurvivesCopy 验收 4：把 data/ 复制到另一个路径
// （mtime 全变），数据指纹不变。
//
// 这是「哈希内容而不是 mtime」的核心理由。用 mtime 的话，
// 复制一份数据指纹就变了 —— 而那明明是同一份数据。
func TestDataFingerprintSurvivesCopy(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	fakeData(t, d1)
	a, _ := fp(t, d1)

	// 复制到另一个目录，并把 mtime 全部改掉
	fakeData(t, d2)
	future := time.Now().Add(48 * time.Hour)
	_ = filepath.WalkDir(d2, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			_ = os.Chtimes(p, future, future)
		}
		return nil
	})
	b, _ := fp(t, d2)

	if a != b {
		t.Fatalf("换个目录 + 改 mtime 后指纹变了 —— 说明哈希的不是内容：\n  %s\n  %s", a, b)
	}
}

// TestDataFingerprintDetectsContentChange 改一个字节，指纹必须变。
func TestDataFingerprintDetectsContentChange(t *testing.T) {
	dir := t.TempDir()
	fakeData(t, dir)
	a, _ := fp(t, dir)

	p := filepath.Join(dir, "bar/market=ashare/freq=1d/year=2025/part-00000.parquet")
	if err := os.WriteFile(p, []byte("BARS-2025!"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := fp(t, dir)
	if a == b {
		t.Fatal("改了内容指纹却没变")
	}
}

// TestDataFingerprintIgnoresDerived CSV 镜像、ETL 进度、缓存都不该进指纹。
func TestDataFingerprintIgnoresDerived(t *testing.T) {
	dir := t.TempDir()
	fakeData(t, dir)
	a, _ := fp(t, dir)

	for _, rel := range []string{"meta/instruments.csv", "meta/_sync_state.json", "results/run.csv"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte("变了"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := fp(t, dir); a != b {
		t.Error("派生物 / 进度文件进了指纹 —— 它们不影响回测结果")
	}
}

// TestCacheDoesNotChangeResult 缓存只是为了跳过重算，
// 有没有缓存算出来的指纹必须一致。
func TestCacheDoesNotChangeResult(t *testing.T) {
	dir := t.TempDir()
	fakeData(t, dir)
	a, _ := fp(t, dir) // 第一次：无缓存，全量哈希

	if _, err := os.Stat(filepath.Join(dir, cacheName)); err != nil {
		t.Fatalf("应当写出缓存文件：%v", err)
	}
	b, _ := fp(t, dir) // 第二次：走缓存

	if err := os.Remove(filepath.Join(dir, cacheName)); err != nil {
		t.Fatal(err)
	}
	c, _ := fp(t, dir) // 第三次：缓存删掉，重算

	if a != b || b != c {
		t.Errorf("缓存改变了结果：\n  无缓存 %s\n  走缓存 %s\n  重算   %s", a, b, c)
	}
}

// TestCorruptCacheIsIgnored 缓存文件坏掉时应当退化为重算，而不是报错或给错数。
func TestCorruptCacheIsIgnored(t *testing.T) {
	dir := t.TempDir()
	fakeData(t, dir)
	a, _ := fp(t, dir)

	if err := os.WriteFile(filepath.Join(dir, cacheName), []byte("{ 坏掉的 json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := fp(t, dir); a != b {
		t.Error("缓存损坏时应当重算并给出同一个指纹")
	}
}

func TestEmptyDirIsAnError(t *testing.T) {
	if _, _, err := Data(t.TempDir()); err == nil {
		t.Error("空目录应当报错 —— 静默给出一个指纹会掩盖「数据没准备好」")
	}
}

// TestEngineVersionAndReproducible dev 构建必须被标为不可复现。
func TestEngineVersionAndReproducible(t *testing.T) {
	old := EngineVersion()
	defer SetEngineVersion(old)

	SetEngineVersion("")
	if EngineVersion() != "dev" || Reproducible() {
		t.Error("空版本应当退化为 dev 且标记不可复现")
	}
	SetEngineVersion("abc123")
	if !Reproducible() {
		t.Error("注入了 commit 就该标记为可复现")
	}
}

// TestHexIsPrefixFree 分段长度前缀防止拼接歧义。
//
// 没有它，("ab","c") 与 ("a","bc") 会摘出同一个指纹 ——
// 于是「改了配置但数据指纹相应变化」这类组合可能撞车。
func TestHexIsPrefixFree(t *testing.T) {
	if Hex([]byte("ab"), []byte("c")) == Hex([]byte("a"), []byte("bc")) {
		t.Error("分段拼接有歧义")
	}
}

func TestShort(t *testing.T) {
	if got := Short("0123456789abcdef"); got != "0123456789ab" {
		t.Errorf("期望 12 位，得到 %q", got)
	}
	if got := Short("abc"); got != "abc" {
		t.Errorf("短串原样返回，得到 %q", got)
	}
}
