package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
	"gopkg.in/yaml.v3"
)

// ================== Config Struct ==================
type Config struct {
	EmbyServer      string    `yaml:"emby_server"`
	HideRealLibrary bool      `yaml:"hide_real_library"`
	Library         []Library `yaml:"library"`
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

func getAllCollections(boxId string, orignalResp *http.Response) []map[string]interface{} {
	userId := strings.Split(orignalResp.Request.URL.Path, "/")[3]
	token := orignalResp.Request.URL.Query().Get("X-Emby-Token")
	url := config.EmbyServer + "/emby/Users/" + userId + "/Items"
	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	query := orignalResp.Request.URL.Query()
	query.Set("ParentId", boxId)
	query.Set("X-Emby-Token", token)
	query.Set("X-Emby-Language", orignalResp.Request.Header.Get("Accept-Language"))
	req.URL.RawQuery = query.Encode()
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	var data map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
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
	userId := strings.Split(orignalResp.Request.URL.Path, "/")[3]
	token := orignalResp.Request.URL.Query().Get("X-Emby-Token")
	// http://127.0.0.1:8000/emby/Users/2253db2c33584679b6a7ee38d9616315/Views
	url := config.EmbyServer + "/emby/Users/" + userId + "/Views"
	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	query := orignalResp.Request.URL.Query()
	query.Set("X-Emby-Token", token)
	query.Set("X-Emby-Language", orignalResp.Request.Header.Get("Accept-Language"))
	req.URL.RawQuery = query.Encode()
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	var data map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
	if err != nil {
		return nil
	}
	var boxsets map[string]interface{}
	// find first item with CollectionType is boxsets
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
		return false
	}
	collectionId := boxsets["Id"].(string)
	collections := getAllCollections(collectionId, orignalResp)
	if len(collections) == 0 {
		return false
	}
	for _, collection := range collections {
		if collection["Id"].(string) == id {
			return true
		}
	}
	return false
}

func getCollectionData(id string, orignalResp *http.Response) map[string]interface{} {
	if !ensureCollectionExist(id, orignalResp) {
		emptyCollection := map[string]interface{}{}
		emptyCollection["Items"] = []interface{}{}
		return emptyCollection
	}

	userId := strings.Split(orignalResp.Request.URL.Path, "/")[3]
	client := &http.Client{}
	req, err := http.NewRequest("GET", config.EmbyServer+"/emby/Users/"+userId+"/Items", nil)
	if err != nil {
		return nil
	}
	// ParentId="+id+"&ImageTypeLimit=1&Fields=BasicSyncInfo%2CCanDelete%2CCanDownload%2CPrimaryImageAspectRatio%2CProductionYear%2CStatus%2CEndDate&EnableTotalRecordCount=false&sortBy=DisplayOrder&sortOrder=Ascending&IncludeItemTypes=Movie&X-Emby-Client=Emby+Web&X-Emby-Device-Name=Microsoft+Edge+macOS&X-Emby-Device-Id=213228ff-8f5f-4a63-b042-33b4882223b3&X-Emby-Client-Version=4.8.11.0&X-Emby-Token="+token+"&X-Emby-Language=en-us
	// override query string, SortBy, SortOrder, IncludeItemTypes
	orignalQuery := orignalResp.Request.URL.Query()
	query := req.URL.Query()
	query.Set("ParentId", id)
	query.Set("ImageTypeLimit", orignalQuery.Get("ImageTypeLimit"))
	query.Set("Fields", orignalQuery.Get("Fields"))
	query.Set("EnableTotalRecordCount", orignalQuery.Get("EnableTotalRecordCount"))
	query.Set("SortBy", orignalQuery.Get("SortBy"))
	query.Set("SortOrder", orignalQuery.Get("SortOrder"))
	query.Set("X-Emby-Client", orignalQuery.Get("X-Emby-Client"))
	query.Set("X-Emby-Device-Name", orignalQuery.Get("X-Emby-Device-Name"))
	query.Set("X-Emby-Device-Id", orignalQuery.Get("X-Emby-Device-Id"))
	query.Set("X-Emby-Client-Version", orignalQuery.Get("X-Emby-Client-Version"))
	query.Set("X-Emby-Token", orignalQuery.Get("X-Emby-Token"))
	query.Set("X-Emby-Language", orignalQuery.Get("X-Emby-Language"))
	req.URL.RawQuery = query.Encode()

	req.Header.Set("Accept-Language", orignalResp.Request.Header.Get("Accept-Language"))
	req.Header.Set("User-Agent", orignalResp.Request.Header.Get("User-Agent"))
	req.Header.Set("accept", "application/json")
	// 复制原请求的 Cookie
	for _, c := range orignalResp.Request.Cookies() {
		req.AddCookie(c)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	// 取出 Items 的值
	var data map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
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
	for _, lib := range config.Library {
		if HashNameToID(lib.Name) == tag {
			log.Println("hookImage tag", tag)
			// read image from ./images/
			image, err := os.ReadFile(lib.Image)
			if err != nil {
				return err
			}
			resp.Body = io.NopCloser(bytes.NewReader(image))
			resp.ContentLength = int64(len(image))
			resp.Header.Set("Content-Length", strconv.Itoa(len(image)))
			resp.Header.Del("Content-Encoding")
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
			break
		}
	}
	bodyBytes, err := json.Marshal(data)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	resp.ContentLength = int64(len(bodyBytes))
	resp.Header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
	resp.Header.Del("Content-Encoding")
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
		bodyText := getCollectionData(collectionID, resp)
		bodyBytes, err := json.Marshal(bodyText)
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		resp.ContentLength = int64(len(bodyBytes))
		resp.Header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
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
		items := getCollectionData(collectionID, resp)["Items"].([]interface{})
		bodyBytes, err := json.Marshal(items)
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		resp.ContentLength = int64(len(bodyBytes))
		resp.Header.Set("Content-Length", strconv.Itoa(len(bodyBytes)))
		resp.Header.Del("Content-Encoding")
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
	serverId := items[0].(map[string]interface{})["ServerId"].(string)
	log.Println("Items count", len(data["Items"].([]interface{})))
	// 遍历 config.Library，生成 item
	var newItems []interface{}
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
	if config.HideRealLibrary {
		items = newItems // 只显示虚拟库
	} else {
		items = append(newItems, items...) // 合并
	}
	data["Items"] = items
	newBody, err := json.Marshal(data)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(newBody))
	resp.ContentLength = int64(len(newBody))
	resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	resp.Header.Del("Content-Encoding")
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
		// 添加 X-Forwarded-For
		if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			prior := req.Header.Get("X-Forwarded-For")
			if prior != "" {
				req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
			} else {
				req.Header.Set("X-Forwarded-For", clientIP)
			}
		}
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
