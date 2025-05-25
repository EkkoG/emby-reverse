package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"gopkg.in/yaml.v3"
)

// ================== Config Struct ==================
type Config struct {
	EmbyServer string    `yaml:"emby_server"`
	Hide       []string  `yaml:"hide"`
	Library    []Library `yaml:"library"`
}

type Library struct {
	Name         string `yaml:"name"`
	CollectionID string `yaml:"collection_id"`
	Image        string `yaml:"image"`
}

var config Config

var (
	hookViewsRe       = regexp.MustCompile(`^/emby/Users/[^/]+/Views$`)
	hookLatestRe      = regexp.MustCompile(`^/emby/Users/[^/]+/Items/Latest$`)
	hookDetailsRe     = regexp.MustCompile(`^/emby/Users/[^/]+/Items$`)
	hookDetailIntroRe = regexp.MustCompile(`^/emby/Users/[^/]+/Items/\d+$`)
	hookImageRe       = regexp.MustCompile(`^/emby/Items/\d+/Images/Primary$`)
)

type ResponseHook struct {
	Pattern *regexp.Regexp
	Handler func(*http.Response) error
}

var responseHooks = []ResponseHook{
	{hookViewsRe, hookViews},
	{hookLatestRe, hookLatest},
	{hookDetailsRe, hookDetails},
	{hookDetailIntroRe, hookDetailIntro},
	{hookImageRe, hookImage},
}

const imageDoneFile = "image_done.txt"

var (
	imageOnceMu sync.Mutex
)

// ================== Utility Functions ==================
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg Config
	decoder := yaml.NewDecoder(f)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func HashNameToID(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	return strconv.FormatUint(uint64(h.Sum32()), 10)
}

// Check if id is a hash of any library name in config
func isLibraryHashID(id string) bool {
	for _, lib := range config.Library {
		if HashNameToID(lib.Name) == id {
			return true
		}
	}
	return false
}

// Get collection_id by hash id
func getCollectionIDByHashID(hashID string) (string, bool) {
	for _, lib := range config.Library {
		if HashNameToID(lib.Name) == hashID {
			return lib.CollectionID, true
		}
	}
	return "", false
}

// 获取 userId
func getUserId(resp *http.Response) string {
	parts := strings.Split(resp.Request.URL.Path, "/")
	userId := ""
	if len(parts) > 3 {
		userId = parts[3]
	}
	return userId
}

// 拼接 Emby API URL
func embyURL(path string, userId string) string {
	return config.EmbyServer + strings.Replace(path, "{userId}", userId, 1)
}

// 通用 GET 请求并解析 JSON
func doGetJSON(
	baseURL string,
	query url.Values,
	headers http.Header,
	cookies []*http.Cookie,
) (map[string]interface{}, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", baseURL, nil)
	if err != nil {
		return nil, err
	}
	if query != nil {
		req.URL.RawQuery = query.Encode()
	}
	for k, v := range headers {
		for _, vv := range v {
			req.Header.Add(k, vv)
		}
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

// 优化 X-Emby 参数处理，优先 originalQuery，其次 header，最后 query
func setXEmbyParams(query, originalQuery url.Values, header http.Header) {
	xEmbyKeys := []string{"X-Emby-Client", "X-Emby-Device-Name", "X-Emby-Device-Id", "X-Emby-Client-Version", "X-Emby-Token", "X-Emby-Language"}
	for _, key := range xEmbyKeys {
		val := originalQuery.Get(key)
		if val == "" {
			val = header.Get(key)
		}
		if val != "" {
			query.Set(key, val)
		}
	}
}

func getAllCollections(boxId string, orignalResp *http.Response) []map[string]interface{} {
	userId := getUserId(orignalResp)

	query := url.Values{}
	query.Set("ParentId", boxId)

	setXEmbyParams(query, orignalResp.Request.URL.Query(), orignalResp.Request.Header)

	headers := http.Header{}
	headers.Set("Accept-Language", orignalResp.Request.Header.Get("Accept-Language"))
	headers.Set("User-Agent", orignalResp.Request.Header.Get("User-Agent"))
	headers.Set("accept", "application/json")

	cookies := orignalResp.Request.Cookies()

	url := embyURL("/emby/Users/{userId}/Items", userId)
	data, err := doGetJSON(url, query, headers, cookies)
	if err != nil {
		return nil
	}
	var collections []map[string]interface{}
	for _, item := range data["Items"].([]interface{}) {
		collections = append(collections, item.(map[string]interface{}))
	}
	return collections
}

func getFirstBoxset(orignalResp *http.Response) map[string]interface{} {
	userId := getUserId(orignalResp)

	query := url.Values{}

	setXEmbyParams(query, orignalResp.Request.URL.Query(), orignalResp.Request.Header)
	log.Println("getFirstBoxset query", query)

	headers := http.Header{}
	headers.Set("Accept-Language", orignalResp.Request.Header.Get("Accept-Language"))
	headers.Set("User-Agent", orignalResp.Request.Header.Get("User-Agent"))
	headers.Set("accept", "application/json")

	cookies := orignalResp.Request.Cookies()

	url := embyURL("/emby/Users/{userId}/Views", userId)
	data, err := doGetJSON(url, query, headers, cookies)
	if err != nil {
		return nil
	}
	var boxsets map[string]interface{}
	for _, item := range data["Items"].([]interface{}) {
		if item.(map[string]interface{})["CollectionType"] == "boxsets" {
			boxsets = item.(map[string]interface{})
			break
		}
	}
	if boxsets == nil {
		return nil
	}
	return boxsets
}

func ensureCollectionExist(id string, orignalResp *http.Response) bool {
	boxsets := getFirstBoxset(orignalResp)
	if boxsets == nil {
		log.Println("boxsets is nil")
		return false
	}
	collectionId := boxsets["Id"].(string)
	collections := getAllCollections(collectionId, orignalResp)
	if len(collections) == 0 {
		log.Println("collections is empty")
		return false
	}
	for _, collection := range collections {
		if collection["Id"].(string) == id {
			log.Println("collection exist", id)
			return true
		}
	}
	log.Println("collection not exist", id)
	return false
}

// getCollectionData 增加 sort 参数
func getCollectionData(id string, orignalResp *http.Response, sort *struct{ By, Order string }) map[string]interface{} {
	if !ensureCollectionExist(id, orignalResp) {
		log.Println("collection not exist, will return empty collection", id)
		emptyCollection := map[string]interface{}{}
		emptyCollection["Items"] = []interface{}{}
		return emptyCollection
	}

	orignalQuery := orignalResp.Request.URL.Query()
	query := url.Values{} // 避免污染原始 query
	query.Set("ParentId", id)
	query.Set("ImageTypeLimit", orignalQuery.Get("ImageTypeLimit"))
	query.Set("Fields", orignalQuery.Get("Fields"))
	query.Set("EnableTotalRecordCount", orignalQuery.Get("EnableTotalRecordCount"))
	if sort != nil {
		query.Set("SortBy", sort.By)
		query.Set("SortOrder", sort.Order)
	} else {
		query.Set("SortBy", orignalQuery.Get("SortBy"))
		query.Set("SortOrder", orignalQuery.Get("SortOrder"))
	}

	setXEmbyParams(query, orignalResp.Request.URL.Query(), orignalResp.Request.Header)

	headers := http.Header{}
	headers.Set("Accept-Language", orignalResp.Request.Header.Get("Accept-Language"))
	headers.Set("User-Agent", orignalResp.Request.Header.Get("User-Agent"))
	headers.Set("accept", "application/json")

	cookies := orignalResp.Request.Cookies()

	userId := getUserId(orignalResp)
	url := embyURL("/emby/Users/{userId}/Items", userId)
	data, err := doGetJSON(url, query, headers, cookies)
	if err != nil {
		return nil
	}
	log.Println("getCollectionData data count", len(data["Items"].([]interface{})))
	return data
}

func hookImage(resp *http.Response) error {
	log.Println("hookImage")
	// get tag
	tag := resp.Request.URL.Query().Get("tag")
	if tag == "" {
		tag = resp.Request.URL.Query().Get("Tag")
	}
	for _, lib := range config.Library {
		if HashNameToID(lib.Name) == tag {
			log.Println("hookImage tag", tag)
			var image []byte
			if lib.Image != "" {
				userImage, err := os.ReadFile(lib.Image)
				if err != nil {
					return err
				}
				image = userImage
			} else {
				// image = []byte{}
				path := fmt.Sprintf("images/%s.png", lib.Name)
				// check if file exists
				if _, err := os.Stat(path); os.IsNotExist(err) {
					placeholder, err := os.ReadFile("assets/placeholder.png")
					if err != nil {
						return err
					}
					image = placeholder
				} else {
					image, err = os.ReadFile(path)
					if err != nil {
						return err
					}
				}
			}
			contentType := http.DetectContentType(image)
			encoding := resp.Header.Get("Content-Encoding")
			encodedBody, err := encodeBodyByContentEncoding(image, encoding)
			if err != nil {
				return err
			}
			resp.Body = io.NopCloser(bytes.NewReader(encodedBody))
			resp.ContentLength = int64(len(encodedBody))
			resp.Header.Set("Content-Length", strconv.Itoa(len(encodedBody)))
			resp.Header.Set("Content-Type", contentType)
			if encoding == "" {
				resp.Header.Del("Content-Encoding")
			} else {
				resp.Header.Set("Content-Encoding", encoding)
			}
			resp.StatusCode = 200
			resp.Status = "200 OK"
			return nil
		}
	}
	return nil
}

func hookDetailIntro(resp *http.Response) error {
	template := `{
    "Name": "Sample Library",
    "ServerId": "",
    "Id": "1241",
    "Guid": "470c3d1e3b5e4a0287ad485a5cf67207",
    "Etag": "8281abb37d32a2b95db7e5a5df4407a4",
    "DateCreated": "2025-04-19T09:07:17.0000000Z",
    "CanDelete": false,
    "CanDownload": false,
    "PresentationUniqueKey": "470c3d1e3b5e4a0287ad485a5cf67207",
    "SupportsSync": true,
    "SortName": "Sample Library",
    "ForcedSortName": "Sample Library",
    "ExternalUrls": [],
    "Taglines": [],
    "RemoteTrailers": [],
    "ProviderIds": {},
    "IsFolder": true,
    "ParentId": "1",
    "Type": "CollectionFolder",
    "UserData": {
        "PlaybackPositionTicks": 0,
        "IsFavorite": false,
        "Played": false
    },
    "ChildCount": 1,
    "DisplayPreferencesId": "470c3d1e3b5e4a0287ad485a5cf67207",
    "PrimaryImageAspectRatio": 1.7777777777777777,
    "CollectionType": "tvshows",
    "ImageTags": {
        "Primary": "79219cbf328f6dfc6e2b3ad599233d34"
    },
    "BackdropImageTags": [],
    "LockedFields": [],
    "LockData": false,
    "Subviews": [
        "series",
        "studios",
        "genres",
        "episodes",
        "series",
        "folders"
    ]
}`
	// get id after Items/
	components := strings.Split(resp.Request.URL.Path, "/")
	id := components[len(components)-1]
	if !isLibraryHashID(id) {
		return nil
	}
	log.Println("hookDetailIntro id", id)
	// 获取真实 collection_id
	_, ok := getCollectionIDByHashID(id)
	if !ok {
		return nil
	}
	var data map[string]interface{}
	err := json.Unmarshal([]byte(template), &data)
	if err != nil {
		return err
	}
	// 用库名和 hash id 替换
	for _, lib := range config.Library {
		if HashNameToID(lib.Name) == id {
			data["Name"] = lib.Name
			data["Id"] = id
			data["ImageTags"] = map[string]string{
				"Primary": HashNameToID(lib.Name),
			}
			break
		}
	}
	bodyBytes, err := json.Marshal(data)
	if err != nil {
		return err
	}
	encoding := resp.Header.Get("Content-Encoding")
	encodedBody, err := encodeBodyByContentEncoding(bodyBytes, encoding)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(encodedBody))
	resp.ContentLength = int64(len(encodedBody))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(encodedBody)))
	if encoding == "" {
		resp.Header.Del("Content-Encoding")
	} else {
		resp.Header.Set("Content-Encoding", encoding)
	}
	resp.StatusCode = 200
	resp.Status = "200 OK"
	return nil
}

func hookDetails(resp *http.Response) error {
	log.Println("hookDetails")
	// get parentId
	parentId := resp.Request.URL.Query().Get("ParentId")
	if isLibraryHashID(parentId) {
		collectionID, ok := getCollectionIDByHashID(parentId)
		if !ok {
			return nil
		}
		log.Println("collectionID", collectionID)
		bodyText := getCollectionData(collectionID, resp, nil)
		bodyBytes, err := json.Marshal(bodyText)
		if err != nil {
			return err
		}
		encoding := resp.Header.Get("Content-Encoding")
		encodedBody, err := encodeBodyByContentEncoding(bodyBytes, encoding)
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(encodedBody))
		resp.ContentLength = int64(len(encodedBody))
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Set("Content-Length", strconv.Itoa(len(encodedBody)))
		if encoding == "" {
			resp.Header.Del("Content-Encoding")
		} else {
			resp.Header.Set("Content-Encoding", encoding)
		}
		return nil
	}
	return nil
}

func hookLatest(resp *http.Response) error {
	log.Println("hookLatest")
	// get parentId
	parentId := resp.Request.URL.Query().Get("ParentId")
	if isLibraryHashID(parentId) {
		collectionID, ok := getCollectionIDByHashID(parentId)
		if !ok {
			return nil
		}
		log.Println("collectionID", collectionID)
		order := struct {
			By    string
			Order string
		}{
			By:    "DateCreated,SortName",
			Order: "Descending",
		}
		items := getCollectionData(collectionID, resp, &order)["Items"].([]interface{})
		bodyBytes, err := json.Marshal(items)
		if err != nil {
			return err
		}
		encoding := resp.Header.Get("Content-Encoding")
		encodedBody, err := encodeBodyByContentEncoding(bodyBytes, encoding)
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(encodedBody))
		resp.ContentLength = int64(len(encodedBody))
		resp.Header.Set("Content-Length", strconv.Itoa(len(encodedBody)))
		if encoding == "" {
			resp.Header.Del("Content-Encoding")
		} else {
			resp.Header.Set("Content-Encoding", encoding)
		}
		return nil
	}
	return nil
}

func hookViews(resp *http.Response) error {
	template := `{
		"BackdropImageTags": [],
		"CanDelete": false,
		"CanDownload": false,
		"ChildCount": 1,
		"CollectionType": "tvshows",
		"DateCreated": "2025-04-19T09:07:17.0000000Z",
		"DisplayPreferencesId": "470c3d1e3b5e4a0287ad485a5cf67207",
		"Etag": "8281abb37d32a2b95db7e5a5df4407a4",
		"ExternalUrls": [],
		"ForcedSortName": "Sample Library",
		"Guid": "470c3d1e3b5e4a0287ad485a5cf67207",
		"Id": "1241",
		"ImageTags": {
			"Primary": "79219cbf328f6dfc6e2b3ad599233d34"
		},
		"IsFolder": true,
		"LockData": false,
		"LockedFields": [],
		"Name": "Sample Library",
		"ParentId": "1",
		"PresentationUniqueKey": "470c3d1e3b5e4a0287ad485a5cf67207",
		"PrimaryImageAspectRatio": 1.7777777777777777,
		"ProviderIds": {},
		"RemoteTrailers": [],
		"ServerId": "",
		"SortName": "Sample Library",
		"Taglines": [],
		"Type": "CollectionFolder",
		"UserData": {
			"IsFavorite": false,
			"PlaybackPositionTicks": 0,
			"Played": false
		}
	}`
	log.Println("hookViews")
	var bodyBytes []byte
	var err error
	log.Println("resp.Header.Get(Content-Encoding)", resp.Header.Get("Content-Encoding"))
	if resp.Header.Get("Content-Encoding") == "br" {
		br := brotli.NewReader(resp.Body)
		bodyBytes, err = io.ReadAll(br)
		resp.Body.Close()
	} else if resp.Header.Get("Content-Encoding") == "deflate" {
		df := flate.NewReader(resp.Body)
		bodyBytes, err = io.ReadAll(df)
		resp.Body.Close()
	} else if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			log.Println("gzip.NewReader error", err)
		}
		bodyBytes, err = io.ReadAll(gz)
		if err != nil {
			log.Println("io.ReadAll error", err)
		}
		resp.Body.Close()
	} else {
		bodyBytes, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err != nil {
		return err
	}
	var data map[string]interface{}
	err = json.Unmarshal(bodyBytes, &data)
	if err != nil {
		log.Println("json.Unmarshal error", err)
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return nil
	}
	items, ok := data["Items"].([]interface{})
	if !ok {
		items = []interface{}{}
	}
	if len(items) == 0 {
		return nil
	}
	typedItems := make([]map[string]interface{}, 0)
	for _, item := range items {
		typedItems = append(typedItems, item.(map[string]interface{}))
	}
	serverId := typedItems[0]["ServerId"].(string)
	log.Println("Items count", len(typedItems))
	// 遍历 config.Library，生成 item
	var newItems []map[string]interface{}
	for _, lib := range config.Library {
		var item map[string]interface{}
		err := json.Unmarshal([]byte(template), &item)
		if err != nil {
			continue
		}
		item["Name"] = lib.Name
		item["SortName"] = lib.Name
		item["ForcedSortName"] = lib.Name
		item["Id"] = HashNameToID(lib.Name)
		item["ImageTags"] = map[string]string{
			"Primary": HashNameToID(lib.Name),
		}
		item["ServerId"] = serverId
		newItems = append(newItems, item)
	}
	// 根据配置决定是否合并真实库
	if slices.Contains(config.Hide, "all") {
		typedItems = []map[string]interface{}{}
	} else {
		// items = newItems // 只显示虚拟库
		oldItems := []map[string]interface{}{}
		for _, item := range typedItems {
			if len(config.Hide) > 0 {
				if slices.Contains(config.Hide, item["CollectionType"].(string)) {
					continue
				} else {
					oldItems = append(oldItems, item)
				}
			}
		}
		typedItems = oldItems
	}
	typedItems = append(newItems, typedItems...) // 合并
	data["Items"] = typedItems
	newBody, err := json.Marshal(data)
	if err != nil {
		return err
	}
	encoding := resp.Header.Get("Content-Encoding")
	encodedBody, err := encodeBodyByContentEncoding(newBody, encoding)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(encodedBody))
	resp.ContentLength = int64(len(encodedBody))
	resp.Header.Set("Content-Length", strconv.Itoa(len(encodedBody)))
	if encoding == "" {
		resp.Header.Del("Content-Encoding")
	} else {
		resp.Header.Set("Content-Encoding", encoding)
	}

	// 异步获取图片
	for _, lib := range config.Library {
		libCopy := lib // 防止闭包变量问题
		go func(l Library) {
			// 这里可以只传递 l 和必要的参数
			// 比如 userId、token 等
			// getImage 需要调整为接收这些参数
			err := getImage(&l, resp)
			if err != nil {
				log.Println("getImage error", err)
			}
		}(libCopy)
	}

	return nil
}

func modifyResponse(resp *http.Response) error {
	for _, hook := range responseHooks {
		if hook.Pattern.MatchString(resp.Request.URL.Path) {
			log.Println("matched", resp.Request.URL.Path)
			log.Println("hook", hook.Pattern.String())
			return hook.Handler(resp)
		}
	}
	return nil
}

func encodeBodyByContentEncoding(body []byte, encoding string) ([]byte, error) {
	var buf bytes.Buffer
	switch encoding {
	case "gzip":
		gz := gzip.NewWriter(&buf)
		_, err := gz.Write(body)
		if err != nil {
			return nil, err
		}
		gz.Close()
		return buf.Bytes(), nil
	case "deflate":
		df, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		_, err = df.Write(body)
		if err != nil {
			return nil, err
		}
		df.Close()
		return buf.Bytes(), nil
	case "br":
		br := brotli.NewWriter(&buf)
		_, err := br.Write(body)
		if err != nil {
			return nil, err
		}
		br.Close()
		return buf.Bytes(), nil
	default:
		return body, nil // 不压缩
	}
}

func loadImageDone() map[string]bool {
	done := make(map[string]bool)
	data, err := os.ReadFile(imageDoneFile)
	if err != nil {
		return done // 文件不存在视为都没处理过
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if line != "" {
			done[line] = true
		}
	}
	return done
}

func saveImageDone(libName string) error {
	f, err := os.OpenFile(imageDoneFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(libName + "\n")
	return err
}

func getImage(lib *Library, orignalResp *http.Response) error {
	imageOnceMu.Lock()
	imageDone := loadImageDone()
	if imageDone[lib.Name] {
		imageOnceMu.Unlock()
		log.Println("image done, skip", lib.Name)
		return nil // 已经处理过
	}
	imageOnceMu.Unlock()

	items := getCollectionData(lib.CollectionID, orignalResp, nil)["Items"].([]interface{})
	// 随机选择 9 个
	for i := 0; i < 9; i++ {
		randomIndex := rand.Intn(len(items))
		imageId := items[randomIndex].(map[string]interface{})["ImageTags"].(map[string]interface{})["Primary"].(string)
		itemId := items[randomIndex].(map[string]interface{})["Id"].(string)
		imageUrl := fmt.Sprintf("%s/emby/Items/%s/Images/Primary?maxHeight=600&maxWidth=400&tag=%s&quality=90", config.EmbyServer, itemId, imageId)
		image, err := http.Get(imageUrl)
		if err != nil {
			return err
		}
		imageBytes, err := io.ReadAll(image.Body)
		image.Body.Close()
		if err != nil {
			return err
		}
		os.MkdirAll(fmt.Sprintf("images/%s", lib.Name), 0755)
		err = os.WriteFile(fmt.Sprintf("images/%s/%d.jpg", lib.Name, i + 1), imageBytes, 0644)
		if err != nil {
			return err
		}
	}
	// uv run python gen.py
	// call cmd to gen image
	cmd := exec.Command("uv", "run", "python", "cover_gen.py", lib.Name)
	cmd.Dir = "."
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return err
	}

	// 只有全部成功后才记录
	imageOnceMu.Lock()
	err = saveImageDone(lib.Name)
	imageOnceMu.Unlock()
	return err
}

func main() {
	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		log.Println("LoadConfig error", err)
		return
	}
	config = *cfg

	target, err := url.Parse(config.EmbyServer)
	if err != nil {
		log.Println("url.Parse error", err)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	// 修改 Director 保证 Host 头正确
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host

		// 获取客户端IP
		clientIP, _, _ := net.SplitHostPort(req.RemoteAddr)
		if clientIP != "" {
			// X-Forwarded-For
			prior := req.Header.Get("X-Forwarded-For")
			if prior != "" {
				req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
			} else {
				req.Header.Set("X-Forwarded-For", clientIP)
			}
			// X-Real-IP
			req.Header.Set("X-Real-IP", clientIP)
		}

		// X-Forwarded-Protocol
		scheme := "http"
		if req.TLS != nil {
			scheme = "https"
		}
		req.Header.Set("X-Forwarded-Protocol", scheme)
	}

	// 修改响应，处理重定向
	proxy.ModifyResponse = func(resp *http.Response) error {
		return modifyResponse(resp)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})

	log.Println("emby-virtual-lib listen on :8000")
	http.ListenAndServe(":8000", nil)
}
