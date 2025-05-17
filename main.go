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
	EmbyServer string    `yaml:"emby_server"`
	Library    []Library `yaml:"library"`
}

type Library struct {
	Name         string `yaml:"name"`
	CollectionID string `yaml:"collection_id"`
	Image        string `yaml:"image"`
}

var config Config

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

func getCollectionData(id string, orignalResp *http.Response) map[string]interface{} {
	userId := strings.Split(orignalResp.Request.URL.Path, "/")[3]
	token := orignalResp.Request.URL.Query().Get("X-Emby-Token")
	client := &http.Client{}
	req, err := http.NewRequest("GET", config.EmbyServer+"/emby/Users/"+userId+"/Items?ParentId="+id+"&ImageTypeLimit=1&Fields=BasicSyncInfo%2CCanDelete%2CCanDownload%2CPrimaryImageAspectRatio%2CProductionYear%2CStatus%2CEndDate&EnableTotalRecordCount=false&sortBy=DisplayOrder&sortOrder=Ascending&IncludeItemTypes=Movie&X-Emby-Client=Emby+Web&X-Emby-Device-Name=Microsoft+Edge+macOS&X-Emby-Device-Id=213228ff-8f5f-4a63-b042-33b4882223b3&X-Emby-Client-Version=4.8.11.0&X-Emby-Token="+token+"&X-Emby-Language=en-us", nil)
	if err != nil {
		return nil
	}
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
	if strings.HasPrefix(resp.Request.URL.Path, "/emby/Items/") && strings.HasSuffix(resp.Request.URL.Path, "/Images/Primary") {
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
	}
	return nil
}

func hookDetailIntro(resp *http.Response) error {
	template := `{
    "Name": "Sample Library",
    "ServerId": "a9d52db3cb7b41c5958642fe456d411e",
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
	re := regexp.MustCompile(`/emby/Users/[^/]+/Items/\d+`)
	if re.MatchString(resp.Request.URL.Path) {
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
	return nil
}

func hookDetails(resp *http.Response) error {
	if strings.HasPrefix(resp.Request.URL.Path, "/emby/Users/") && strings.HasSuffix(resp.Request.URL.Path, "/Items") {
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
	}
	return nil
}

func hookLatest(resp *http.Response) error {
	if strings.HasPrefix(resp.Request.URL.Path, "/emby/Users/") && strings.HasSuffix(resp.Request.URL.Path, "/Items/Latest") {
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
		"ServerId": "a9d52db3cb7b41c5958642fe456d411e",
		"SortName": "Sample Library",
		"Taglines": [],
		"Type": "CollectionFolder",
		"UserData": {
			"IsFavorite": false,
			"PlaybackPositionTicks": 0,
			"Played": false
		}
	}`
	if strings.HasPrefix(resp.Request.URL.Path, "/emby/Users/") && strings.HasSuffix(resp.Request.URL.Path, "/Views") {
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
			newItems = append(newItems, item)
		}
		// 合并新老 items
		items = append(newItems, items...)
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
	return nil
}

func modifyResponse(resp *http.Response) error {
	hookViews(resp)
	hookLatest(resp)
	hookDetails(resp)
	hookDetailIntro(resp)
	hookImage(resp)
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
