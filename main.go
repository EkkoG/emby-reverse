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
	"time"

	"github.com/andybalholm/brotli"
	"github.com/dgraph-io/badger/v4"
	"gopkg.in/yaml.v3"
)

// ================== Config Struct ==================
type Config struct {
	EmbyServer string    `yaml:"emby_server"`
	EmbyApiKey string    `yaml:"emby_api_key"`
	Hide       []string  `yaml:"hide"`
	Library    []Library `yaml:"library"`
}

type Library struct {
	Name         string `yaml:"name"`
	ResourceID   string `yaml:"resource_id"`
	ResourceType string `yaml:"resource_type"`
	Image        string `yaml:"image"`
}

func (l *Library) NeedRecursive() bool {
	return l.ResourceType != "collection"
}

// 返回参数名
func (l *Library) GetParamKey() string {
	switch l.ResourceType {
	case "collection":
		return "ParentId"
	case "tag":
		return "TagIds"
	case "genre":
		return "GenreIds"
	case "studio":
		return "StudioIds"
	case "person":
		return "PersonIds"
	default:
		return ""
	}
}

var config Config
var libraryMap = map[string]Library{}

var (
	hookViewsRe       = regexp.MustCompile(`/Users/[^/]+/Views$`)
	hookLatestRe      = regexp.MustCompile(`/Users/[^/]+/Items/Latest$`)
	hookDetailsRe     = regexp.MustCompile(`/Users/[^/]+/Items$`)
	hookDetailIntroRe = regexp.MustCompile(`/Users/[^/]+/Items/\d+$`)
	hookImageRe       = regexp.MustCompile(`/Items/\d+/Images/Primary$`)
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

var badgerDB *badger.DB

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

// 获取 userId
func getUserId(req *http.Request) string {
	path := req.URL.Path
	parts := strings.Split(path, "/")
	userId := ""
	if parts[1] == "emby" {
		if len(parts) > 3 {
			userId = parts[3]
		}
	} else {
		if len(parts) > 2 {
			userId = parts[2]
		}
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
	xEmbyKeys := []string{"X-Emby-Client", "X-Emby-Device-Name", "X-Emby-Device-Id", "X-Emby-Client-Version", "X-Emby-Token", "X-Emby-Language", "X-Emby-Authorization"}
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

func getAllCollections(boxId string, orignalReq *http.Request) []map[string]interface{} {
	userId := getUserId(orignalReq)

	query := url.Values{}
	query.Set("ParentId", boxId)

	setXEmbyParams(query, orignalReq.URL.Query(), orignalReq.Header)

	headers := http.Header{}
	headers.Set("Accept-Language", orignalReq.Header.Get("Accept-Language"))
	headers.Set("User-Agent", orignalReq.Header.Get("User-Agent"))
	headers.Set("accept", "application/json")

	cookies := orignalReq.Cookies()

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

func getFirstBoxset(orignalReq *http.Request) map[string]interface{} {
	userId := getUserId(orignalReq)

	query := url.Values{}

	setXEmbyParams(query, orignalReq.URL.Query(), orignalReq.Header)

	headers := http.Header{}
	headers.Set("Accept-Language", orignalReq.Header.Get("Accept-Language"))
	headers.Set("User-Agent", orignalReq.Header.Get("User-Agent"))
	headers.Set("accept", "application/json")

	cookies := orignalReq.Cookies()

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

func ensureCollectionExist(id string, orignalReq *http.Request) bool {
	boxsets := getFirstBoxset(orignalReq)
	if boxsets == nil {
		log.Println("boxsets is nil")
		return false
	}
	collectionId := boxsets["Id"].(string)
	collections := getAllCollections(collectionId, orignalReq)
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

func getCollectionDataWithApi(lib Library, apiKey string) map[string]interface{} {
	query := url.Values{}
	if lib.GetParamKey() != "" && lib.ResourceID != "" {
		query.Set(lib.GetParamKey(), lib.ResourceID)
	}
	query.Set("ImageTypeLimit", "1")
	if lib.NeedRecursive() {
		query.Set("Recursive", "true")
	}
	query.Set("Fields", "BasicSyncInfo,CanDelete,CanDownload,PrimaryImageAspectRatio,ProductionYear,Status,EndDate")
	query.Set("EnableTotalRecordCount", "true")
	query.Set("API_KEY", apiKey)

	url := fmt.Sprintf("%s/emby/Items", config.EmbyServer)
	headers := http.Header{}
	headers.Set("accept", "application/json")
	data, err := doGetJSON(url, query, headers, nil)
	if err != nil {
		return nil
	}
	return data
}

// getItems 增加 type 参数，自动根据 id 字段选择参数名
func getItems(lib Library, orignalReq *http.Request, extQuery url.Values) map[string]interface{} {
	orignalQuery := orignalReq.URL.Query()
	query := url.Values{} // 避免污染原始 query

	if lib.GetParamKey() != "" && lib.ResourceID != "" {
		query.Set(lib.GetParamKey(), lib.ResourceID)
	}
	log.Println("getItems query", query)

	query.Set("IncludeItemTypes", "Movie,Series,Video,Game,MusicAlbum")
	query.Set("ImageTypeLimit", orignalQuery.Get("ImageTypeLimit"))
	query.Set("Fields", orignalQuery.Get("Fields"))
	query.Set("EnableTotalRecordCount", orignalQuery.Get("EnableTotalRecordCount"))
	if lib.NeedRecursive() {
		query.Set("Recursive", "true")
	}
	if extQuery != nil {
		for k, v := range extQuery {
			query.Set(k, v[0])
		}
	} else {
		query.Set("SortBy", orignalQuery.Get("SortBy"))
		query.Set("SortOrder", orignalQuery.Get("SortOrder"))
	}

	setXEmbyParams(query, orignalReq.URL.Query(), orignalReq.Header)

	headers := http.Header{}
	headers.Set("Accept-Language", orignalReq.Header.Get("Accept-Language"))
	headers.Set("User-Agent", orignalReq.Header.Get("User-Agent"))
	headers.Set("accept", "application/json")

	cookies := orignalReq.Cookies()

	userId := getUserId(orignalReq)
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
	lib, ok := libraryMap[tag]
	if !ok {
		return nil
	}
	log.Println("hookImage tag", tag)
	var image []byte
	if lib.Image != "" {
		userImage, err := os.ReadFile(lib.Image)
		if err != nil {
			return err
		}
		image = userImage
		// 设置缓存响应头
		resp.Header.Set("Cache-Control", "public, max-age=86400")
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
			resp.Header.Set("Cache-Control", "public, max-age=86400")
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
	lib, ok := libraryMap[id]
	if !ok {
		return nil
	}
	log.Println("hookDetailIntro id", id)
	var data map[string]interface{}
	err := json.Unmarshal([]byte(template), &data)
	if err != nil {
		return err
	}
	// 用库名和 hash id 替换
	data["Name"] = lib.Name
	data["Id"] = id
	data["ImageTags"] = map[string]string{
		"Primary": id,
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
	parentId := resp.Request.URL.Query().Get("ParentId")
	lib, ok := libraryMap[parentId]
	if !ok {
		return nil
	}
	bodyText := getItems(lib, resp.Request, nil)
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

func hookLatest(resp *http.Response) error {
	log.Println("hookLatest")
	start := time.Now()
	parentId := resp.Request.URL.Query().Get("ParentId")
	lib, ok := libraryMap[parentId]
	if !ok {
		return nil
	}
	query := url.Values{}
	query.Set("SortBy", "DateCreated,SortName")
	query.Set("SortOrder", "Descending")
	query.Set("Limit", resp.Request.URL.Query().Get("Limit"))
	query.Set("IsPlayed", "false")
	if lib.NeedRecursive() {
		query.Set("Recursive", "true")
	}
	log.Println("before getCollectionData")
	getDataStart := time.Now()
	items := getItems(lib, resp.Request, query)["Items"].([]interface{})
	log.Printf("getCollectionData done, cost: %v, items: %d", time.Since(getDataStart), len(items))
	marshalStart := time.Now()
	bodyBytes, err := json.Marshal(items)
	log.Printf("json.Marshal done, cost: %v", time.Since(marshalStart))
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
	log.Printf("hookLatest total cost: %v", time.Since(start))
	return nil
}

func hookViews(resp *http.Response) error {
	template := `{
		"BackdropImageTags": [],
		"CanDelete": false,
		"CanDownload": false,
		"ChildCount": 1,
		"CollectionType": "VirtualLibrary",
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
	if len(config.Hide) == 0 {
		// do nothing
	} else {
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

	return nil
}

func modifyResponse(resp *http.Response) error {
	for _, hook := range responseHooks {
		if hook.Pattern.MatchString(resp.Request.URL.Path) {
			log.Println("matched", resp.Request.URL.Path)
			log.Println("hook", hook.Pattern.String())
			log.Println("hook start", resp.Request.URL.Path)
			hookStart := time.Now()
			err := hook.Handler(resp)
			log.Printf("hook %s cost: %v", resp.Request.URL.Path, time.Since(hookStart))
			return err
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

func getImage(lib *Library) error {
	alreadyGenerated := false
	err := badgerDB.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(lib.Name))
		if err != nil {
			// 只在不是not found时打印
			if err != badger.ErrKeyNotFound {
				log.Println("badgerDB.View error", err)
			}
			return nil
		}
		return item.Value(func(val []byte) error {
			if string(val) == "1" {
				log.Println("badgerDB.View item", lib.Name, "already generated")
				alreadyGenerated = true
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	fileName := fmt.Sprintf("images/%s.png", lib.Name)
	fileExist, err := os.Stat(fileName)
	if alreadyGenerated && err == nil && fileExist.Size() > 0 {
		return nil
	}
	log.Println("cover gen start", lib.Name)

	items := getCollectionDataWithApi(*lib, config.EmbyApiKey)["Items"].([]interface{})
	itemCount := len(items)
	if itemCount == 0 {
		log.Println("no available image", lib.Name)
		return nil // 没有可用图片
	}

	var selected []interface{}
	if itemCount <= 9 {
		selected = items
	} else {
		// 洗牌
		rand.Shuffle(itemCount, func(i, j int) { items[i], items[j] = items[j], items[i] })
		selected = items[:9]
	}

	for i, itemRaw := range selected {
		item := itemRaw.(map[string]interface{})
		imageTags, ok := item["ImageTags"].(map[string]interface{})
		if !ok {
			continue
		}
		imageId, ok := imageTags["Primary"].(string)
		if !ok {
			continue
		}
		itemId, ok := item["Id"].(string)
		if !ok {
			continue
		}
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
		err = os.WriteFile(fmt.Sprintf("images/%s/%d.jpg", lib.Name, i+1), imageBytes, 0644)
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
	err = cmd.Run()
	if err != nil {
		return err
	}

	badgerDB.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(lib.Name), []byte("1"))
	})
	return err
}

func main() {
	// 初始化 Badger
	var err error
	badgerDB, err = badger.Open(badger.DefaultOptions("images/badger_db").WithLogger(nil))
	if err != nil {
		log.Println("badger open error", err)
		return
	}
	defer badgerDB.Close()

	cfg, err := LoadConfig("config.yaml")
	if err != nil {
		log.Println("LoadConfig error", err)
		return
	}
	config = *cfg
	for _, lib := range config.Library {
		libraryMap[HashNameToID(lib.Name)] = lib
	}

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

	// 异步获取图片
	for _, lib := range config.Library {
		libCopy := lib // 防止闭包变量问题
		go func(l Library) {
			// 这里可以只传递 l 和必要的参数
			// 比如 userId、token 等
			// getImage 需要调整为接收这些参数
			err := getImage(&l)
			if err != nil {
				log.Println("getImage error", err)
			}
		}(libCopy)
	}

	log.Println("emby-virtual-lib listen on :8000")
	http.ListenAndServe(":8000", nil)
}
