// 流光逸影 - 冷逸的局域网私人影院
// 双击运行后，手机浏览器访问 http://<电脑IP>:<端口> 即可浏览并播放电脑里的电影。
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/qr.html
var qrHTML []byte

//go:embed web/pc.html
var pcHTML []byte

//go:embed web/bg.jpg
var bgJPG []byte

type Config struct {
	Dirs  []string `json:"dirs"`  // 电影目录列表
	Port  int      `json:"port"`  // 监听端口
	Token string   `json:"token"` // 访问口令，留空表示不需要
}

type Root struct {
	Name string // 对外显示的名字（取目录 basename，重名加序号）
	Abs  string // 绝对路径
	Cfg  string // config.json 中的原始写法（用于删除时回写）
}

type ProbeInfo struct {
	Duration   float64 `json:"duration"`
	VideoCodec string  `json:"videoCodec"`
	AudioCodec string  `json:"audioCodec"`
	HasAudio   bool    `json:"hasAudio"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	DirectPlay bool    `json:"directPlay"`
	MSECodecs  string  `json:"mseCodecs"`
}

var (
	cfg      Config
	roots    []Root
	rootsMu  sync.RWMutex
	exeDir   string
	ffmpeg   string
	ffprobe  string
	probeMu  sync.Mutex
	probeMap = map[string]*ProbeInfo{}
)

var videoExts = map[string]bool{
	".mp4": true, ".m4v": true, ".mkv": true, ".webm": true,
	".avi": true, ".mov": true, ".ts": true, ".flv": true,
	".wmv": true, ".rmvb": true, ".mpg": true, ".mpeg": true, ".3gp": true,
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	exeDir = filepath.Dir(exe)

	loadConfig()
	rebuildRoots()
	if len(roots) == 0 {
		log.Fatal("没有可用的共享目录，请检查 config.json")
	}
	findFFmpeg()

	mux := http.NewServeMux()
	registerDLNA(mux)
	go startSSDP()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/qr", handleQRPage)
	mux.HandleFunc("/qr.png", handleQRPNG)
	mux.HandleFunc("/bg.jpg", handleBG)
	mux.HandleFunc("/pc", localOnly(handlePC))
	mux.HandleFunc("/api/browse", localOnly(auth(handleBrowse)))
	mux.HandleFunc("/api/list", auth(handleList))
	mux.HandleFunc("/api/probe", auth(handleProbe))
	mux.HandleFunc("/api/roots", auth(handleRoots))
	mux.HandleFunc("/api/roots/add", auth(handleRootAdd))
	mux.HandleFunc("/api/roots/remove", auth(handleRootRemove))
	mux.HandleFunc("/stream", auth(handleStream))
	mux.HandleFunc("/transcode", auth(handleTranscode))

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	url := lanURL()

	fmt.Println("============================================")
	fmt.Println("  流光逸影 已启动 —— 冷逸的私人影院")
	fmt.Printf("  手机浏览器访问: %s\n", url)
	fmt.Printf("  电脑控制台(选电影文件夹): http://localhost:%d/pc\n", cfg.Port)
	fmt.Println("  电视/盒子: 在自带「媒体中心/文件管理」的 DLNA 列表里找「流光逸影」")
	if cfg.Token != "" {
		fmt.Println("  已启用访问口令")
	}
	if ffmpeg == "" {
		fmt.Println("  [提示] 未找到 ffmpeg，MKV 等格式将无法网页播放（可用 VLC 打开）")
	}
	fmt.Println("============================================")

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 端口被占用说明已有一个实例在跑，友好提示而不是莫名其妙闪退
		fmt.Println("============================================")
		fmt.Println("  流光逸影已经在运行中，无需重复启动")
		fmt.Printf("  电脑控制台: http://localhost:%d/pc\n", cfg.Port)
		fmt.Println("============================================")
		openBrowser(fmt.Sprintf("http://localhost:%d/pc", cfg.Port))
		fmt.Println("按回车关闭本窗口...")
		fmt.Scanln()
		return
	}
	openBrowser(fmt.Sprintf("http://localhost:%d/pc", cfg.Port))
	log.Fatal(http.Serve(ln, mux))
}

func loadConfig() {
	p := filepath.Join(exeDir, "config.json")
	data, err := os.ReadFile(p)
	if err != nil {
		cfg = Config{Dirs: []string{"."}, Port: 8090}
		if out, merr := json.MarshalIndent(cfg, "", "  "); merr == nil {
			_ = os.WriteFile(p, out, 0644)
			fmt.Printf("已生成默认配置文件 %s，可编辑后重启生效\n", p)
		}
		return
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("config.json 解析失败: %v", err)
	}
	if cfg.Port == 0 {
		cfg.Port = 8090
	}
	if len(cfg.Dirs) == 0 {
		cfg.Dirs = []string{"."}
	}
}

func saveConfig() error {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(exeDir, "config.json"), out, 0644)
}

// absDir 把配置里的目录（相对 exe 目录或绝对路径）解析为绝对路径。
func absDir(d string) string {
	if filepath.IsAbs(d) {
		return filepath.Clean(d)
	}
	abs, err := filepath.Abs(filepath.Join(exeDir, d))
	if err != nil {
		return ""
	}
	return abs
}

func rebuildRoots() {
	rootsMu.Lock()
	defer rootsMu.Unlock()
	roots = nil
	seen := map[string]int{}
	for _, d := range cfg.Dirs {
		abs := absDir(d)
		if abs == "" {
			continue
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			fmt.Printf("[警告] 目录不存在，已跳过: %s\n", d)
			continue
		}
		name := filepath.Base(abs)
		if vol := filepath.VolumeName(abs); abs == vol+`\` {
			name = vol + "盘" // 整盘共享时显示为 "G盘"
		}
		seen[name]++
		if seen[name] > 1 {
			name = fmt.Sprintf("%s (%d)", name, seen[name])
		}
		roots = append(roots, Root{Name: name, Abs: abs, Cfg: d})
		fmt.Printf("共享目录: %s  ->  %s\n", name, abs)
	}
	libUpdateID.Add(1) // 目录结构变化，让 DLNA 客户端感知刷新
}

func findFFmpeg() {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		ffmpeg = p
	} else if st, err := os.Stat(filepath.Join(exeDir, "ffmpeg.exe")); err == nil && !st.IsDir() {
		ffmpeg = filepath.Join(exeDir, "ffmpeg.exe")
	}
	if p, err := exec.LookPath("ffprobe"); err == nil {
		ffprobe = p
	} else if st, err := os.Stat(filepath.Join(exeDir, "ffprobe.exe")); err == nil && !st.IsDir() {
		ffprobe = filepath.Join(exeDir, "ffprobe.exe")
	}
}

func lanIP() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ip := ipnet.IP.To4(); ip != nil && ip.IsPrivate() {
				return ip.String()
			}
		}
	}
	return "127.0.0.1"
}

func lanURL() string {
	u := fmt.Sprintf("http://%s:%d", lanIP(), cfg.Port)
	if cfg.Token != "" {
		u += "/?key=" + cfg.Token
	}
	return u
}

func openBrowser(url string) {
	_ = exec.Command("cmd", "/c", "start", url).Start()
}

// ---------- 安全与鉴权 ----------

func auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.Token != "" && r.URL.Query().Get("key") != cfg.Token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// resolve 把虚拟路径（如 "电影/动作片/a.mkv"）解析为磁盘绝对路径，并阻止目录穿越。
func resolve(vpath string) (string, error) {
	vpath = strings.Trim(strings.TrimPrefix(vpath, "/"), "/")
	if vpath == "" {
		return "", fmt.Errorf("根目录无对应磁盘路径")
	}
	segs := strings.SplitN(vpath, "/", 2)
	rootsMu.RLock()
	defer rootsMu.RUnlock()
	var root *Root
	for i := range roots {
		if roots[i].Name == segs[0] {
			root = &roots[i]
			break
		}
	}
	if root == nil {
		return "", fmt.Errorf("未知根目录: %s", segs[0])
	}
	abs := root.Abs
	if len(segs) == 2 {
		abs = filepath.Join(root.Abs, filepath.FromSlash(segs[1]))
	}
	abs = filepath.Clean(abs)
	// 整盘共享时 root.Abs 自带结尾分隔符（如 G:\），先去掉再拼，否则所有子路径都会被误判
	base := strings.TrimSuffix(root.Abs, string(os.PathSeparator))
	if abs != base && !strings.HasPrefix(abs, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("非法路径")
	}
	return abs, nil
}

// ---------- 页面 ----------

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func handleQRPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := strings.ReplaceAll(string(qrHTML), "{{URL}}", lanURL())
	w.Write([]byte(html))
}

func handleQRPNG(w http.ResponseWriter, r *http.Request) {
	png, err := qrcode.Encode(lanURL(), qrcode.Medium, 320)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(png)
}

func handleBG(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(bgJPG)
}

// ---------- 电脑端控制台（仅本机可访问） ----------

// localOnly 只允许来自本机的请求，防止局域网其他设备浏览整块硬盘。
func localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			http.Error(w, `{"error":"仅允许本机访问"}`, http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func handlePC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := strings.ReplaceAll(string(pcHTML), "{{URL}}", lanURL())
	html = strings.ReplaceAll(html, "{{KEY}}", cfg.Token)
	w.Write([]byte(html))
}

// handleBrowse 供电脑端控制台用鼠标浏览本机磁盘：空路径返回盘符列表，否则返回子文件夹列表。
func handleBrowse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	p := strings.TrimSpace(r.URL.Query().Get("path"))

	type browseEntry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}

	if p == "" {
		drives := make([]browseEntry, 0, 4)
		for c := 'C'; c <= 'Z'; c++ {
			d := string(c) + `:\`
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				drives = append(drives, browseEntry{Name: d, Path: d})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"path": "", "entries": drives})
		return
	}

	abs := filepath.Clean(p)
	infos, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, `{"error":"无法打开该文件夹（可能无权限）"}`, 400)
		return
	}
	entries := make([]browseEntry, 0, len(infos))
	for _, fi := range infos {
		if !fi.IsDir() {
			continue
		}
		name := fi.Name()
		// 跳过隐藏目录和 Windows 系统目录
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") || strings.EqualFold(name, "System Volume Information") {
			continue
		}
		entries = append(entries, browseEntry{Name: name, Path: filepath.Join(abs, name)})
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	parent := filepath.Dir(abs)
	if parent == abs {
		parent = ""
	}
	json.NewEncoder(w).Encode(map[string]any{"path": abs, "parent": parent, "entries": entries})
}

// ---------- API ----------

type Entry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
	IsVideo bool   `json:"isVideo"`
}

// listDirEntries 列出目录（跳过隐藏/系统项，目录在前、名称排序），网页端与 DLNA 共用。
func listDirEntries(abs string) ([]Entry, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	infos, err := f.Readdir(-1)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(infos))
	for _, fi := range infos {
		name := fi.Name()
		// 跳过隐藏项和 Windows 系统目录
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") || strings.EqualFold(name, "System Volume Information") {
			continue
		}
		e := Entry{Name: name, IsDir: fi.IsDir(), Size: fi.Size(), ModTime: fi.ModTime().Unix()}
		if !fi.IsDir() {
			e.IsVideo = videoExts[strings.ToLower(filepath.Ext(name))]
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

func handleList(w http.ResponseWriter, r *http.Request) {
	vpath := r.URL.Query().Get("path")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if vpath == "" || vpath == "/" {
		rootsMu.RLock()
		list := make([]Entry, 0, len(roots))
		for _, rt := range roots {
			list = append(list, Entry{Name: rt.Name, IsDir: true})
		}
		rootsMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]any{"path": "", "parent": "", "entries": list})
		return
	}

	abs, err := resolve(vpath)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, 400)
		return
	}
	entries, err := listDirEntries(abs)
	if err != nil {
		http.Error(w, `{"error":"读取目录失败"}`, 500)
		return
	}

	parent := ""
	if idx := strings.LastIndex(vpath, "/"); idx > 0 {
		parent = vpath[:idx]
	}
	json.NewEncoder(w).Encode(map[string]any{"path": vpath, "parent": parent, "entries": entries})
}

// ---------- 共享文件夹管理 ----------

func jsonOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write([]byte(`{"ok":true}`))
}

func handleRoots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	type rootInfo struct {
		Name string `json:"name"`
		Dir  string `json:"dir"`
	}
	rootsMu.RLock()
	list := make([]rootInfo, 0, len(roots))
	for _, rt := range roots {
		list = append(list, rootInfo{Name: rt.Name, Dir: rt.Abs})
	}
	rootsMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]any{"roots": list})
}

func handleRootAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, 405)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		http.Error(w, `{"error":"请输入文件夹路径"}`, 400)
		return
	}
	p := strings.TrimSpace(req.Path)
	if !filepath.IsAbs(p) {
		http.Error(w, `{"error":"请输入完整路径（如 D:\\电影）"}`, 400)
		return
	}
	abs := filepath.Clean(p)
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		http.Error(w, `{"error":"文件夹不存在: `+p+`"}`, 400)
		return
	}
	for _, d := range cfg.Dirs {
		if da := absDir(d); da != "" && strings.EqualFold(da, abs) {
			http.Error(w, `{"error":"该文件夹已在列表中"}`, 400)
			return
		}
	}
	cfg.Dirs = append(cfg.Dirs, abs)
	rebuildRoots()
	if err := saveConfig(); err != nil {
		http.Error(w, `{"error":"保存配置失败"}`, 500)
		return
	}
	fmt.Printf("新增共享目录: %s\n", abs)
	jsonOK(w)
}

func handleRootRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, 405)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"参数错误"}`, 400)
		return
	}
	rootsMu.RLock()
	idx := -1
	var target Root
	for i := range roots {
		if roots[i].Name == req.Name {
			idx = i
			target = roots[i]
			break
		}
	}
	total := len(roots)
	rootsMu.RUnlock()
	if idx < 0 {
		http.Error(w, `{"error":"未找到该文件夹"}`, 400)
		return
	}
	if total <= 1 {
		http.Error(w, `{"error":"至少保留一个共享文件夹"}`, 400)
		return
	}
	for i, d := range cfg.Dirs {
		if d == target.Cfg {
			cfg.Dirs = append(cfg.Dirs[:i], cfg.Dirs[i+1:]...)
			break
		}
	}
	rebuildRoots()
	if err := saveConfig(); err != nil {
		http.Error(w, `{"error":"保存配置失败"}`, 500)
		return
	}
	fmt.Printf("移除共享目录: %s\n", target.Abs)
	jsonOK(w)
}

// ---------- ffprobe ----------

type ffprobeOut struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Profile   string `json:"profile"`
		Level     int    `json:"level"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func probe(abs string) (*ProbeInfo, error) {
	probeMu.Lock()
	if pi, ok := probeMap[abs]; ok {
		probeMu.Unlock()
		return pi, nil
	}
	probeMu.Unlock()

	if ffprobe == "" {
		return nil, fmt.Errorf("ffprobe 不可用")
	}
	cmd := exec.Command(ffprobe, "-v", "error",
		"-show_entries", "format=duration:stream=codec_type,codec_name,profile,level,width,height",
		"-of", "json", abs)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe 执行失败: %v", err)
	}
	var fo ffprobeOut
	if err := json.Unmarshal(out, &fo); err != nil {
		return nil, err
	}

	pi := &ProbeInfo{}
	pi.Duration, _ = strconv.ParseFloat(fo.Format.Duration, 64)
	for _, s := range fo.Streams {
		switch s.CodecType {
		case "video":
			if pi.VideoCodec == "" {
				pi.VideoCodec = s.CodecName
				pi.Width, pi.Height = s.Width, s.Height
				pi.MSECodecs = h264CodecsString(s.Profile, s.Level)
			}
		case "audio":
			if pi.AudioCodec == "" {
				pi.AudioCodec = s.CodecName
				pi.HasAudio = true
			}
		}
	}

	ext := strings.ToLower(filepath.Ext(abs))
	browserContainer := ext == ".mp4" || ext == ".m4v" || ext == ".webm"
	audioOK := !pi.HasAudio || pi.AudioCodec == "aac" || pi.AudioCodec == "mp3"
	videoOK := pi.VideoCodec == "h264" || pi.VideoCodec == "vp8" || pi.VideoCodec == "vp9"
	pi.DirectPlay = browserContainer && videoOK && audioOK

	if pi.MSECodecs == "" {
		pi.MSECodecs = "avc1.640033" // 转码输出 H.264 High
	}
	if pi.HasAudio {
		pi.MSECodecs += ", mp4a.40.2"
	}

	probeMu.Lock()
	probeMap[abs] = pi
	probeMu.Unlock()
	return pi, nil
}

// cachedProbe 只查缓存不触发 ffprobe，供 DLNA 列表用（避免浏览大目录时逐个探测拖慢响应）
func cachedProbe(abs string) *ProbeInfo {
	probeMu.Lock()
	defer probeMu.Unlock()
	return probeMap[abs]
}

// h264CodecsString 根据 profile/level 生成 avc1.xxxx 形式的 codec 描述。
func h264CodecsString(profile string, level int) string {
	var p string
	switch {
	case strings.HasPrefix(profile, "High"):
		p = "6400"
	case strings.HasPrefix(profile, "Main"):
		p = "4D40"
	default:
		p = "42E0"
	}
	if level <= 0 {
		level = 51
	}
	return fmt.Sprintf("avc1.%s%02x", p, level)
}

func handleProbe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	abs, err := resolve(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, 400)
		return
	}
	pi, err := probe(abs)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, 503)
		return
	}
	json.NewEncoder(w).Encode(pi)
}

// ---------- 流式播放 ----------

var videoMIME = map[string]string{
	".mkv": "video/x-matroska", ".mp4": "video/mp4", ".m4v": "video/mp4",
	".webm": "video/webm", ".avi": "video/x-msvideo", ".mov": "video/quicktime",
	".ts": "video/mp2t", ".flv": "video/x-flv", ".wmv": "video/x-ms-wmv",
	".rmvb": "application/vnd.rn-realmedia-vbr", ".mpg": "video/mpeg",
	".mpeg": "video/mpeg", ".3gp": "video/3gpp",
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	abs, err := resolve(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, "无法打开文件", 404)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "无法读取文件", 500)
		return
	}
	ext := strings.ToLower(filepath.Ext(abs))
	if ct := videoMIME[ext]; ct != "" {
		w.Header().Set("Content-Type", ct)
	} else if ct := mime.TypeByExtension(ext); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	// ServeContent 自动处理 Range 请求（拖动进度条的关键）
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

// ---------- 转封装/转码（MSE 播放用） ----------

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

func handleTranscode(w http.ResponseWriter, r *http.Request) {
	if ffmpeg == "" {
		http.Error(w, "ffmpeg 不可用", 503)
		return
	}
	abs, err := resolve(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	start, _ := strconv.ParseFloat(r.URL.Query().Get("start"), 64)
	pi, _ := probe(abs) // 失败也继续，按保守策略全转码

	args := []string{"-hide_banner", "-loglevel", "error"}
	if start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64))
	}
	args = append(args, "-i", abs)

	// 视频：H.264 直接拷贝（零 CPU），其余转码
	if pi != nil && pi.VideoCodec == "h264" {
		args = append(args, "-c:v", "copy")
	} else {
		// 短 GOP（约 2 秒一个关键帧），拖动进度条后更快出画面
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-crf", "23",
			"-g", "48", "-keyint_min", "24", "-sc_threshold", "0")
	}
	// 音频：AAC 直接拷贝，其余转 AAC
	if pi != nil && !pi.HasAudio {
		args = append(args, "-an")
	} else if pi != nil && pi.AudioCodec == "aac" {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "160k", "-ac", "2")
	}
	// -ss 定位后关键帧前的包带负时间戳，MP4 封装器会报错，归零处理
	args = append(args, "-avoid_negative_ts", "make_zero")
	// 输出碎片化 MP4，供浏览器 MSE 逐步喂入
	args = append(args, "-f", "mp4", "-movflags", "frag_keyframe+empty_moov+default_base_moof", "pipe:1")

	cmd := exec.Command(ffmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, "ffmpeg 启动失败: "+err.Error(), 500)
		return
	}

	// 手机端断开连接时杀掉 ffmpeg，避免残留进程
	go func() {
		<-r.Context().Done()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	fw := flushWriter{w, w.(http.Flusher)}
	io.CopyBuffer(fw, stdout, make([]byte, 64*1024))
	stdout.Close()
	cmd.Wait()
}
