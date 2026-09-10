// DLNA/UPnP MediaServer：SSDP 发现 + ContentDirectory Browse。
// 电视/盒子无需浏览器，在自带「媒体中心/文件管理」的 DLNA 列表里即可发现 流光逸影 并原码率播放（走 /stream，支持 Range 拖动）。
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	ssdpGroup   = "239.255.255.250:1900"
	ssdpMaxAge  = 1800
	ntRoot      = "upnp:rootdevice"
	ntDevice    = "urn:schemas-upnp-org:device:MediaServer:1"
	ntCD        = "urn:schemas-upnp-org:service:ContentDirectory:1"
	ntCM        = "urn:schemas-upnp-org:service:ConnectionManager:1"
	serverAgent = "Windows/10 UPnP/1.0 LiuGuangYiYing/1.0"
)

var (
	dlnaUUID    string
	libUpdateID atomic.Uint32
)

func init() {
	h, err := os.Hostname()
	if err != nil {
		h = "liuguangyiyi"
	}
	// 由主机名派生稳定 UUID：电视记住的是同一台服务器，不会每次重启都变成"新设备"
	sum := sha1.Sum([]byte("liuguangyiyi-media-server:" + h))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	s := hex.EncodeToString(sum[:])
	dlnaUUID = s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// registerDLNA 挂载 DLNA 路由（与网页共用同一端口）。DLNA 协议没有口令机制，故不走 auth。
func registerDLNA(mux *http.ServeMux) {
	mux.HandleFunc("/dlna/description.xml", handleDlnaDescription)
	mux.HandleFunc("/dlna/ContentDirectory.xml", handleDlnaSCPDCD)
	mux.HandleFunc("/dlna/ConnectionManager.xml", handleDlnaSCPDCM)
	mux.HandleFunc("/dlna/ctrl/ContentDirectory", handleDlnaControlCD)
	mux.HandleFunc("/dlna/ctrl/ConnectionManager", handleDlnaControlCM)
	mux.HandleFunc("/dlna/event/ContentDirectory", handleDlnaEvent)
	mux.HandleFunc("/dlna/event/ConnectionManager", handleDlnaEvent)
}

// ---------- SSDP 发现 ----------

// startSSDP 监听 SSDP 组播。注意不能用 net.ListenMulticastUDP——它在 Windows 上收不到
// 组播（实测连本机回环的 NOTIFY 都收不到，电视的 M-SEARCH 也会漏掉），必须通配绑定
// 端口后再用 ipv4.PacketConn.JoinGroup 逐网卡加入组播组。
func startSSDP() {
	group := &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900}
	// SO_REUSEADDR 必须在 bind 前设置（ListenConfig.Control 是唯一注入点），
	// 否则独占端口会挡住 Windows 的 SSDP Discovery 服务和本机其他 DLNA 工具
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var opErr error
			err := c.Control(func(fd uintptr) {
				opErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			})
			if err != nil {
				return err
			}
			return opErr
		},
	}
	ln, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:1900")
	if err != nil {
		fmt.Printf("[DLNA] SSDP 端口监听失败，电视自动发现不可用: %v\n", err)
		return
	}
	pc := ipv4.NewPacketConn(ln.(*net.UDPConn))
	_ = pc.SetMulticastLoopback(true)
	joined := 0
	ifis, _ := net.Interfaces()
	for i := range ifis {
		ifi := &ifis[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if pc.JoinGroup(ifi, group) == nil {
			joined++
		}
	}
	if joined == 0 && pc.JoinGroup(nil, group) != nil {
		fmt.Println("[DLNA] 加入组播组失败，电视自动发现不可用")
		return
	}
	fmt.Printf("[DLNA] SSDP 就绪（已加入 %d 张网卡的组播）\n", joined)
	go ssdpServe(pc)
	go ssdpAnnounceLoop(pc)
}

func ssdpServe(pc *ipv4.PacketConn) {
	buf := make([]byte, 4096)
	for {
		n, _, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		req := string(buf[:n])
		if os.Getenv("DLNA_DEBUG") != "" {
			fmt.Printf("[SSDP] 收到来自 %s: %s\n", from, strings.SplitN(req, "\r\n", 2)[0])
		}
		if !strings.HasPrefix(req, "M-SEARCH") {
			continue
		}
		if man := ssdpHeader(req, "MAN"); man != "" && !strings.Contains(man, "ssdp:discover") {
			continue
		}
		for _, target := range searchTargets(ssdpHeader(req, "ST")) {
			resp := fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=%d\r\nEXT:\r\nLOCATION: http://%s:%d/dlna/description.xml\r\nSERVER: %s\r\nST: %s\r\nUSN: %s\r\n\r\n",
				ssdpMaxAge, lanIP(), cfg.Port, serverAgent, target, usnFor(target))
			pc.WriteTo([]byte(resp), nil, from)
		}
	}
}

func ssdpAnnounceLoop(pc *ipv4.PacketConn) {
	// 启动时先密集广播几轮让电视立刻看到，之后每 5 分钟续命（max-age 的一半以上）
	for i := 0; i < 4; i++ {
		announceAll(pc)
		time.Sleep(700 * time.Millisecond)
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		announceAll(pc)
	}
}

func announceAll(pc *ipv4.PacketConn) {
	for _, nt := range notifyTargets() {
		packet := fmt.Sprintf("NOTIFY * HTTP/1.1\r\nHOST: %s\r\nCACHE-CONTROL: max-age=%d\r\nLOCATION: http://%s:%d/dlna/description.xml\r\nNT: %s\r\nNTS: ssdp:alive\r\nSERVER: %s\r\nUSN: %s\r\n\r\n",
			ssdpGroup, ssdpMaxAge, lanIP(), cfg.Port, nt, serverAgent, usnFor(nt))
		if _, err := pc.WriteTo([]byte(packet), nil, &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900}); err != nil && os.Getenv("DLNA_DEBUG") != "" {
			fmt.Printf("[SSDP] NOTIFY 发送失败 (%s): %v\n", nt, err)
		}
	}
}

func notifyTargets() []string {
	return []string{ntRoot, "uuid:" + dlnaUUID, ntDevice, ntCD, ntCM}
}

func usnFor(nt string) string {
	if nt == "uuid:"+dlnaUUID {
		return nt
	}
	return "uuid:" + dlnaUUID + "::" + nt
}

func searchTargets(st string) []string {
	switch {
	case st == "", st == "ssdp:all":
		return notifyTargets()
	case strings.EqualFold(st, ntRoot), strings.EqualFold(st, ntDevice),
		strings.EqualFold(st, ntCD), strings.EqualFold(st, ntCM), st == "uuid:"+dlnaUUID:
		return []string{st}
	}
	return nil
}

func ssdpHeader(req, name string) string {
	for _, line := range strings.Split(req, "\r\n") {
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(line[:i]), name) {
			return strings.TrimSpace(line[i+1:])
		}
	}
	return ""
}

// ---------- 设备描述与 SCPD ----------

func handleDlnaDescription(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
<specVersion><major>1</major><minor>0</minor></specVersion>
<device>
<deviceType>%s</deviceType>
<friendlyName>流光逸影</friendlyName>
<manufacturer>冷逸</manufacturer>
<modelName>流光逸影</modelName>
<UDN>uuid:%s</UDN>
<serviceList>
<service><serviceType>%s</serviceType><serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId><SCPDURL>/dlna/ContentDirectory.xml</SCPDURL><controlURL>/dlna/ctrl/ContentDirectory</controlURL><eventSubURL>/dlna/event/ContentDirectory</eventSubURL></service>
<service><serviceType>%s</serviceType><serviceId>urn:upnp-org:serviceId:ConnectionManager</serviceId><SCPDURL>/dlna/ConnectionManager.xml</SCPDURL><controlURL>/dlna/ctrl/ConnectionManager</controlURL><eventSubURL>/dlna/event/ConnectionManager</eventSubURL></service>
</serviceList>
</device>
</root>`, ntDevice, dlnaUUID, ntCD, ntCM)
}

const scpdCD = `<?xml version="1.0" encoding="utf-8"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
<specVersion><major>1</major><minor>0</minor></specVersion>
<actionList>
<action><name>Browse</name><argumentList>
<argument><name>ObjectID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable></argument>
<argument><name>BrowseFlag</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_BrowseFlag</relatedStateVariable></argument>
<argument><name>Filter</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Filter</relatedStateVariable></argument>
<argument><name>StartingIndex</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Index</relatedStateVariable></argument>
<argument><name>RequestedCount</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
<argument><name>SortCriteria</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SortCriteria</relatedStateVariable></argument>
<argument><name>Result</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable></argument>
<argument><name>NumberReturned</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
<argument><name>TotalMatches</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
<argument><name>UpdateID</name><direction>out</direction><relatedStateVariable>SystemUpdateID</relatedStateVariable></argument>
</argumentList></action>
<action><name>GetSearchCapabilities</name><argumentList><argument><name>SearchCaps</name><direction>out</direction><relatedStateVariable>SearchCapabilities</relatedStateVariable></argument></argumentList></action>
<action><name>GetSortCapabilities</name><argumentList><argument><name>SortCaps</name><direction>out</direction><relatedStateVariable>SortCapabilities</relatedStateVariable></argument></argumentList></action>
<action><name>GetSystemUpdateID</name><argumentList><argument><name>Id</name><direction>out</direction><relatedStateVariable>SystemUpdateID</relatedStateVariable></argument></argumentList></action>
</actionList>
<serviceStateTable>
<stateVariable sendEvents="yes"><name>SystemUpdateID</name><dataType>ui4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>SearchCapabilities</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>SortCapabilities</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ObjectID</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_BrowseFlag</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Filter</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_SortCriteria</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Result</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Index</name><dataType>ui4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Count</name><dataType>ui4</dataType></stateVariable>
</serviceStateTable>
</scpd>`

const scpdCM = `<?xml version="1.0" encoding="utf-8"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
<specVersion><major>1</major><minor>0</minor></specVersion>
<actionList>
<action><name>GetProtocolInfo</name><argumentList>
<argument><name>Source</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ProtocolInfo</relatedStateVariable></argument>
<argument><name>Sink</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ProtocolInfo</relatedStateVariable></argument>
</argumentList></action>
</actionList>
<serviceStateTable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ProtocolInfo</name><dataType>string</dataType></stateVariable>
</serviceStateTable>
</scpd>`

func handleDlnaSCPDCD(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Write([]byte(scpdCD))
}

func handleDlnaSCPDCM(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Write([]byte(scpdCM))
}

// ---------- SOAP 控制 ----------

func handleDlnaControlCD(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	body, _ := io.ReadAll(r.Body)
	action := soapActionName(r)
	if action == "" {
		// 个别客户端不带 SOAPACTION 头，从报文里认动作
		s := string(body)
		switch {
		case strings.Contains(s, ":Browse"):
			action = "Browse"
		case strings.Contains(s, ":GetSortCapabilities"):
			action = "GetSortCapabilities"
		case strings.Contains(s, ":GetSearchCapabilities"):
			action = "GetSearchCapabilities"
		case strings.Contains(s, ":GetSystemUpdateID"):
			action = "GetSystemUpdateID"
		}
	}
	switch action {
	case "Browse":
		var req struct {
			ObjectID       string `xml:"Body>Browse>ObjectID"`
			BrowseFlag     string `xml:"Body>Browse>BrowseFlag"`
			StartingIndex  int    `xml:"Body>Browse>StartingIndex"`
			RequestedCount int    `xml:"Body>Browse>RequestedCount"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			soapFault(w, "无法解析 Browse 请求")
			return
		}
		objs, err := dlnaBrowseObjects(req.ObjectID, req.BrowseFlag)
		if err != nil {
			soapFault(w, err.Error())
			return
		}
		total := len(objs)
		start := req.StartingIndex
		if start < 0 {
			start = 0
		}
		if start > total {
			start = total
		}
		end := total
		if req.RequestedCount > 0 && start+req.RequestedCount < total {
			end = start + req.RequestedCount
		}
		page := objs[start:end]
		inner := fmt.Sprintf("<Result>%s</Result><NumberReturned>%d</NumberReturned><TotalMatches>%d</TotalMatches><UpdateID>%d</UpdateID>",
			xmlEsc(didlXML(page)), len(page), total, libUpdateID.Load())
		soapResp(w, "ContentDirectory", "Browse", inner)
	case "GetSearchCapabilities":
		soapResp(w, "ContentDirectory", "GetSearchCapabilities", "<SearchCaps></SearchCaps>")
	case "GetSortCapabilities":
		soapResp(w, "ContentDirectory", "GetSortCapabilities", "<SortCaps>+dc:title</SortCaps>")
	case "GetSystemUpdateID":
		soapResp(w, "ContentDirectory", "GetSystemUpdateID", fmt.Sprintf(`<Id val="%d"/>`, libUpdateID.Load()))
	default:
		soapFault(w, "未知动作: "+action)
	}
}

func handleDlnaControlCM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	body, _ := io.ReadAll(r.Body)
	action := soapActionName(r)
	if action == "" && strings.Contains(string(body), "GetProtocolInfo") {
		action = "GetProtocolInfo"
	}
	switch action {
	case "GetProtocolInfo":
		soapResp(w, "ConnectionManager", "GetProtocolInfo", "<Source></Source><Sink></Sink>")
	default:
		soapFault(w, "未知动作: "+action)
	}
}

func soapActionName(r *http.Request) string {
	a := strings.Trim(r.Header.Get("Soapaction"), `"`)
	if i := strings.LastIndex(a, "#"); i >= 0 {
		return strings.TrimSpace(a[i+1:])
	}
	return strings.TrimSpace(a)
}

func soapResp(w http.ResponseWriter, service, action, inner string) {
	resp := `<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:` + action + `Response xmlns:u="urn:schemas-upnp-org:service:` + service + `:1">` + inner + `</u:` + action + `Response></s:Body></s:Envelope>`
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Write([]byte(resp))
}

func soapFault(w http.ResponseWriter, msg string) {
	resp := `<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>401</errorCode><errorDescription>` + xmlEsc(msg) + `</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte(resp))
}

// handleDlnaEvent 只接受订阅请求让电视端满意，不真正推送事件（电视靠重新 Browse 刷新）
func handleDlnaEvent(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "SUBSCRIBE":
		if r.Header.Get("Sid") == "" {
			w.Header().Set("SID", "uuid:"+genUUID())
		}
		w.Header().Set("TIMEOUT", "SECOND-1800")
		w.WriteHeader(http.StatusOK)
	case "UNSUBSCRIBE":
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func genUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// ---------- 内容目录 ----------

type dlnaObject struct {
	id         string
	parentID   string
	title      string
	isDir      bool
	size       int64
	mime       string
	resURL     string
	duration   string
	resolution string
}

func dlnaBrowseObjects(objectID, flag string) ([]dlnaObject, error) {
	if objectID == "" {
		objectID = "0"
	}
	if flag == "BrowseMetadata" {
		if objectID == "0" {
			return []dlnaObject{{id: "0", parentID: "0", title: "流光逸影", isDir: true}}, nil
		}
		siblings, err := dlnaList(dlnaParentOf(objectID))
		if err != nil {
			return nil, err
		}
		for _, o := range siblings {
			if o.id == objectID {
				return []dlnaObject{o}, nil
			}
		}
		return nil, fmt.Errorf("对象不存在: %s", objectID)
	}
	return dlnaList(objectID)
}

func dlnaList(objectID string) ([]dlnaObject, error) {
	if objectID == "" {
		objectID = "0"
	}
	if objectID == "0" {
		rootsMu.RLock()
		objs := make([]dlnaObject, 0, len(roots))
		for _, rt := range roots {
			objs = append(objs, dlnaObject{id: rt.Name, parentID: "0", title: rt.Name, isDir: true})
		}
		rootsMu.RUnlock()
		return objs, nil
	}
	abs, err := resolve(objectID)
	if err != nil {
		return nil, err
	}
	entries, err := listDirEntries(abs)
	if err != nil {
		return nil, fmt.Errorf("无法读取目录")
	}
	objs := make([]dlnaObject, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir && !e.IsVideo {
			continue // DLNA 视图只放文件夹和视频，屏蔽无关文件
		}
		vpath := objectID + "/" + e.Name
		o := dlnaObject{id: vpath, parentID: objectID, title: e.Name, isDir: e.IsDir, size: e.Size}
		if !e.IsDir {
			o.mime = videoMIME[strings.ToLower(filepath.Ext(e.Name))]
			if o.mime == "" {
				o.mime = "application/octet-stream"
			}
			o.resURL = dlnaStreamURL(vpath)
			if pi := cachedProbe(filepath.Join(abs, e.Name)); pi != nil {
				if pi.Duration > 0 {
					o.duration = dlnaDuration(pi.Duration)
				}
				if pi.Width > 0 && pi.Height > 0 {
					o.resolution = fmt.Sprintf("%dx%d", pi.Width, pi.Height)
				}
			}
		}
		objs = append(objs, o)
	}
	return objs, nil
}

func didlXML(objs []dlnaObject) string {
	var b strings.Builder
	b.WriteString(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`)
	for _, o := range objs {
		if o.isDir {
			fmt.Fprintf(&b, `<container id="%s" parentID="%s" restricted="1"><dc:title>%s</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`,
				xmlEsc(o.id), xmlEsc(o.parentID), xmlEsc(o.title))
		} else {
			attrs := fmt.Sprintf(` size="%d"`, o.size)
			if o.duration != "" {
				attrs += ` duration="` + o.duration + `"`
			}
			if o.resolution != "" {
				attrs += ` resolution="` + o.resolution + `"`
			}
			fmt.Fprintf(&b, `<item id="%s" parentID="%s" restricted="1"><dc:title>%s</dc:title><upnp:class>object.item.videoItem</upnp:class><res protocolInfo="http-get:*:%s:DLNA.ORG_OP=01;DLNA.ORG_CI=0"%s>%s</res></item>`,
				xmlEsc(o.id), xmlEsc(o.parentID), xmlEsc(o.title), o.mime, attrs, xmlEsc(o.resURL))
		}
	}
	b.WriteString(`</DIDL-Lite>`)
	return b.String()
}

func dlnaParentOf(objectID string) string {
	if objectID == "" || objectID == "0" {
		return "0"
	}
	if i := strings.LastIndex(objectID, "/"); i > 0 {
		return objectID[:i]
	}
	return "0"
}

func dlnaStreamURL(vpath string) string {
	u := fmt.Sprintf("http://%s:%d/stream?path=%s", lanIP(), cfg.Port, url.QueryEscape(vpath))
	if cfg.Token != "" {
		u += "&key=" + url.QueryEscape(cfg.Token)
	}
	return u
}

func dlnaDuration(sec float64) string {
	s := int(sec + 0.5)
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func xmlEsc(s string) string { return xmlEscaper.Replace(s) }
