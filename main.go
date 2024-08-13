package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"media-server/pkg/config"
	"media-server/pkg/logger"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_115 "media-server/115"

	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var (
	DriveClient *_115.DriveClient
)

func convertToLinuxPath(windowsPath string) string {
	// 将所有的反斜杠转换成正斜杠
	linuxPath := strings.ReplaceAll(windowsPath, "\\", "/")
	return linuxPath
}

func ensureLeadingSlash(alistPath string) string {
	if !strings.HasPrefix(alistPath, "/") {
		alistPath = "/" + alistPath // 不是以 / 开头，加上 /
	}

	alistPath = convertToLinuxPath(alistPath)
	return alistPath
}

func GetItemPathInfo(c *gin.Context) (itemInfoUri string, itemId string, etag string, mediaSourceId string, apiKey string) {
	embyHost := viper.GetString("emby.url")
	embyApiKey := viper.GetString("emby.apikey")
	regex := regexp.MustCompile("[A-Za-z0-9]+")

	// 从URI中解析itemId，移除"emby"和"Sync"，以及所有连字符"-"。
	pathParts := regex.FindAllString(strings.ReplaceAll(strings.ReplaceAll(c.Request.RequestURI, "emby", ""), "Sync", ""), -1)
	if len(pathParts) > 1 {
		itemId = pathParts[1]
	}

	values := c.Request.URL.Query()
	if values.Get("MediaSourceId") != "" {
		mediaSourceId = values.Get("MediaSourceId")
	} else if values.Get("mediaSourceId") != "" {
		mediaSourceId = values.Get("mediaSourceId")
	}
	etag = values.Get("Tag")
	apiKey = values.Get("X-Emby-Token")
	if apiKey == "" {
		apiKey = values.Get("api_key")
	}
	if apiKey == "" {
		apiKey = embyApiKey
	}

	// Construct the itemInfoUri based on the URI and parameters
	if strings.Contains(c.Request.RequestURI, "JobItems") {
		itemInfoUri = embyHost + "/Sync/JobItems?api_key=" + apiKey
	} else {
		if mediaSourceId != "" {
			newMediaSourceId := mediaSourceId
			if strings.HasPrefix(mediaSourceId, "mediasource_") {
				newMediaSourceId = strings.Replace(mediaSourceId, "mediasource_", "", 1)
			}

			itemInfoUri = embyHost + "/Items?Ids=" + newMediaSourceId + "&Fields=Path,MediaSources&Limit=1&api_key=" + apiKey
		} else {
			itemInfoUri = embyHost + "/Items?Ids=" + itemId + "&Fields=Path,MediaSources&Limit=1&api_key=" + apiKey
		}
	}

	return itemInfoUri, itemId, etag, mediaSourceId, apiKey
}

func GetEmbyItems(itemInfoUri string, itemId string, etag string, mediaSourceId string, apiKey string) (map[string]interface{}, error) {
	rvt := map[string]interface{}{
		"message":  "success",
		"protocol": "File",
		"path":     "",
	}

	client := &http.Client{}
	req, err := http.NewRequest("GET", itemInfoUri, nil)
	if err != nil {
		return nil, fmt.Errorf("error: emby_api create request failed, %v", err)
	}
	req.Header.Set("Content-Type", "application/json;charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error: emby_api fetch mediaItemInfo failed, %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		var result map[string]interface{}
		err := json.Unmarshal(bodyBytes, &result)
		if err != nil {
			return nil, fmt.Errorf("error: emby_api response json unmarshal failed, %v", err)
		}

		items, ok := result["Items"].([]interface{})
		if !ok {
			return nil, fmt.Errorf("error: emby_api invalid items format")
		}

		if itemInfoUri[len(itemInfoUri)-9:] == "JobItems" {
			for _, item := range items {
				jobItem := item.(map[string]interface{})
				if jobItem["Id"] == itemId && jobItem["MediaSource"] != nil {
					mediaSource := jobItem["MediaSource"].(map[string]interface{})
					rvt["protocol"] = mediaSource["Protocol"]
					rvt["path"] = mediaSource["Path"]
					return rvt, nil
				}
			}
			rvt["message"] = "error: emby_api /Sync/JobItems response is null"
		} else {
			// Handle case where "MediaType": "Photo"...
			if len(items) > 0 {
				item := items[0].(map[string]interface{})
				rvt["path"] = item["Path"].(string)
				// Parse MediaSources if available
				mediaSources, exists := item["MediaSources"].([]interface{})
				if exists && len(mediaSources) > 0 {
					var mediaSource map[string]interface{}
					for _, source := range mediaSources {
						ms := source.(map[string]interface{})
						if etag != "" && ms["etag"].(string) == etag {
							mediaSource = ms
							break
						}
						if mediaSourceId != "" && ms["Id"].(string) == mediaSourceId {
							mediaSource = ms
							break
						}
					}
					if mediaSource == nil {
						mediaSource = mediaSources[0].(map[string]interface{})
					}
					rvt["protocol"] = mediaSource["Protocol"]
					rvt["path"] = mediaSource["Path"]
				}
				// Decode .strm file path if necessary
				if rvt["path"].(string)[len(rvt["path"].(string))-5:] == ".strm" {
					decodedPath, err := url.QueryUnescape(rvt["path"].(string))
					if err == nil {
						rvt["path"] = decodedPath
					}
				}
			} else {
				rvt["message"] = "error: emby_api /Items response is null"
			}
		}
	} else {
		rvt["message"] = fmt.Sprintf("error: emby_api %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return rvt, nil
}

func extractIDFromPath(path string) (string, error) {
	re := regexp.MustCompile(`/[Vv]ideos/(\S+)/(stream|original|master)`)
	matches := re.FindStringSubmatch(path)
	if len(matches) >= 2 {
		return matches[1], nil
	}
	return "", fmt.Errorf("no match found")
}

func postContent(content string) {

	resp, err := http.PostForm(viper.GetString("server.wx-url"), url.Values{"form": {"text"}, "content": {content}})
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println("Response status:", resp.Status)
}

func syncAndCreateEmptyFiles(sourceDir, remoteDest string) {
	colonIndex := strings.Index(sourceDir, ":")

	// Run rclone sync with the given flags
	cmd := exec.Command("rclone", "sync", sourceDir, filepath.Join(remoteDest, sourceDir[colonIndex+1:]), "-v", "--delete-after", "--size-only", "--ignore-times", "--ignore-existing", "--ignore-checksum", "--max-size", "10M", "--transfers", "10", "--multi-thread-streams", "2", "--local-encoding", "Slash,InvalidUtf8", "--115-encoding", "Slash,InvalidUtf8", "--exclude", "*.strm")
	// 获取命令的标准输出和标准错误的管道 "-vv",
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Println("Error creating StdoutPipe:", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Println("Error creating StderrPipe:", err)
		return
	}

	// 启动命令
	if err := cmd.Start(); err != nil {
		fmt.Println("Error starting command:", err)
		return
	}

	// 创建读取器来实时读取输出
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			fmt.Println("stdout:", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			fmt.Println("Error reading stdout:", err)
		}
	}()

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			re := regexp.MustCompile(`INFO\s+: (.+?): Removing directory`)
			line := scanner.Text()
			matches := re.FindStringSubmatch(line)
			if len(matches) > 1 {
				folderPath := filepath.Join(remoteDest, sourceDir[colonIndex+1:], matches[1])

				if _, err := os.Stat(folderPath); err == nil {
					// 文件夹存在，进行删除
					err = os.RemoveAll(folderPath)
					if err != nil {
						fmt.Printf("Failed to delete folder: %s\n", err)
					} else {
						fmt.Printf("Folder successfully deleted: %s\n", folderPath)
					}
				} else if os.IsNotExist(err) {
					// 文件夹不存在
					fmt.Printf("Folder does not exist: %s\n", folderPath)
				} else {
					// 其他错误
					fmt.Printf("Error checking folder: %v\n", err)
				}
			}
			fmt.Println("stderr:", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			fmt.Println("Error reading stderr:", err)
		}
	}()

	// 等待命令完成
	if err := cmd.Wait(); err != nil {
		fmt.Println("Error waiting for command:", err)
	}

	cmd = exec.Command("rclone", "lsf", "-R", sourceDir, "-vv", "--files-only", "--min-size", "100M", "--transfers", "10", "--multi-thread-streams", "2", "--local-encoding", "Slash,InvalidUtf8", "--115-encoding", "Slash,InvalidUtf8")

	// 获取命令的标准输出管道  "-vv",
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		fmt.Printf("Error creating StdoutPipe: %v\n", err)
		return
	}

	// 启动命令
	if err := cmd.Start(); err != nil {
		fmt.Printf("Error starting command: %v\n", err)
		return
	}

	// 使用 bufio.Scanner 实时读取输出
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		filePath := scanner.Text()
		fileName := filepath.Base(filePath)
		relativePath := filepath.Dir(filePath)

		// 构造目标路径
		destinationPath := filepath.Join(remoteDest, sourceDir[colonIndex+1:], relativePath)
		// fmt.Printf("filePath: %v\n", filePath)
		// fmt.Printf("fileName: %v\n", fileName)
		// fmt.Printf("destinationPath: %v\n", destinationPath)

		// 确保目标路径存在
		err := os.MkdirAll(destinationPath, os.ModePerm)
		if err != nil {
			fmt.Printf("Error creating directories: %v\n", err)
			continue
		}
		outFilePath := filepath.Join(destinationPath, fileName)
		strmFilePath := strings.TrimSuffix(outFilePath, filepath.Ext(outFilePath)) + ".strm"
		if _, err := os.Stat(strmFilePath); os.IsNotExist(err) {
			// 创建 .strm 文件
			file, err := os.OpenFile(strmFilePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0666)
			if err != nil {
				if os.IsExist(err) {
					fmt.Printf("File already exists: %s\n", strmFilePath)
				} else {
					fmt.Printf("Error creating file: %v\n", err)
				}
				return
			}
			defer file.Close()

			// 将 outFilePath 写入 .strm 文件
			_, err = file.WriteString(outFilePath + "\n")
			if err != nil {
				fmt.Printf("Error writing to file: %v\n", err)
				return
			}
			fmt.Printf("Empty file created: %s\n", strmFilePath)
		}
	}

	// 检查命令执行错误
	if err := cmd.Wait(); err != nil {
		fmt.Printf("Error waiting for command: %v\n", err)
	}

	// 检查 bufio.Scanner 错误
	if err := scanner.Err(); err != nil {
		fmt.Printf("Error reading command output: %v\n", err)
	}
}

func mediaFileSync(c *gin.Context) bool {

	clientIP := c.ClientIP()
	fullPath := c.Param("actions")
	re := regexp.MustCompile(`^/sync/(.+)$`)
	matches := re.FindStringSubmatch(fullPath)
	if len(matches) > 1 {
		desiredPath := matches[1]
		// if c.Request.Header.Get("X-Emby-Token") != viper.GetString("emby.apikey") {
		// 	return true
		// }
		if _, found := requestCache.Get(clientIP); found {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"status_code": http.StatusTooManyRequests,
				"message":     "Please wait for 1 minutes before accessing again.",
			})
			return true
		}
		sourceDir := viper.GetString("server.remote") + ":" + desiredPath
		go syncAndCreateEmptyFiles(sourceDir, viper.GetString("server.mount-path"))
		c.String(200, "ok")
		requestCache.Set(clientIP, true, cache.DefaultExpiration)
		return true
	}
	return false
}
func GetRedirectURL(modifiedUrl string, originalHeaders map[string]string) (string, error) {
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // 仅获取重定向URL，不跟随
		},
	}
	req, err := http.NewRequest("GET", modifiedUrl, nil)
	if err != nil {
		return "", err // 创建请求失败
	}
	req.Header.Add("X-Emby-Token", viper.GetString("emby.apikey"))

	for key, value := range originalHeaders {
		req.Header.Add(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err // 发送请求失败
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusFound { // 302
		redirectedURL, err := resp.Location()
		if err != nil {
			return "", err // 获取重定向URL失败
		}
		return redirectedURL.String(), nil
	}

	return "", fmt.Errorf("no redirect or not a 302 status code") // 非302状态码
}

func RemoveQueryParams(originalURL string) string {
	parsedURL, err := url.Parse(originalURL)
	if err != nil {
		return originalURL
	}
	parsedURL.RawQuery = ""
	return parsedURL.String()
}

func ProxyPlaybackInfo(c *gin.Context, proxy *httputil.ReverseProxy) (response map[string]any, skip bool) {
	currentURI := c.Request.RequestURI
	log.Println("当前链接：" + currentURI)
	re := regexp.MustCompile(`/[Ii]tems/(\S+)/PlaybackInfo`)
	matches := re.FindStringSubmatch(currentURI)
	if len(matches) < 1 {
		return nil, true
	}
	// 创建记录器来存储响应内容
	recorder := httptest.NewRecorder()

	// 代理请求
	proxy.ServeHTTP(recorder, c.Request)

	// 处理代理返回结果
	err := json.Unmarshal(recorder.Body.Bytes(), &response)
	if err != nil {
		return response, true
	}

	// 获取代理返回的响应头
	for key, values := range recorder.Header() {
		for _, value := range values {
			if key == "Content-Length" {
				continue
			}
			c.Writer.Header().Set(key, value)
		}
	}

	mediaSources := response["MediaSources"].([]interface{})
	for _, mediaSource := range mediaSources {
		ms := mediaSource.(map[string]interface{})
		log.Println("当前文件路径：" + ms["Path"].(string))
		isCloud := hitReplacePath(ms["Path"].(string))
		if !isCloud {
			log.Println("跳过：不是云盘文件")
			continue
		}
	}
	response["302"] = "true"

	return response, false
}

func hitReplacePath(path string) bool {
	p := viper.GetString("server.mount-path")

	return strings.HasPrefix(path, p)
}

func replaceIgnoreCase(input string, old string, new string) string {
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(old))
	return re.ReplaceAllString(input, new)
}

var requestCache *cache.Cache

func main() {
	config.Init()
	log := logger.Init()
	r := gin.Default()
	log.Info("MEDIA-SERVER-302")
	requestCache = cache.New(10*time.Second, 30*time.Second)
	goCache := cache.New(1*time.Minute, 3*time.Minute)
	embyURL := viper.GetString("emby.url")
	url, _ := url.Parse(embyURL)
	proxy := httputil.NewSingleHostReverseProxy(url)
	cookie := viper.GetString("server.cookie")
	DriveClient = _115.MustNew115DriveClient(cookie)
	r.Any("/*actions", func(c *gin.Context) {
		userAgent := c.Request.Header.Get("User-Agent")
		actions := c.Param("actions")
		logrus.Infoln("Actions:", actions)
		if err := mediaFileSync(c); err {
			c.String(200, actions)
			return
		}

		if strings.HasPrefix(actions, "/ping/") {
			remainingPath := strings.TrimPrefix(actions, "/ping/")
			logrus.Infoln("Remaining Path:", remainingPath)
			c.String(200, "Remaining Path: %s", remainingPath)
			return
		}
		re := regexp.MustCompile(`^/path/(.+)$`)
		matches := re.FindStringSubmatch(actions)
		if len(matches) > 1 {
			desiredPath := matches[1]
			if c.Request.Header.Get("X-Emby-Token") != viper.GetString("emby.apikey") {
				proxy.ServeHTTP(c.Writer, c.Request)
				return
			}
			files, err := DriveClient.GetFile(desiredPath)
			if err != nil {
				proxy.ServeHTTP(c.Writer, c.Request)
				return
			}
			// /aaa/新神榜：哪吒重生/新神榜：哪吒重生.mp4
			down_url, err := DriveClient.GetFileURL(files, userAgent)
			if err != nil {
				proxy.ServeHTTP(c.Writer, c.Request)
				return
			}
			logrus.Infoln(down_url)
			c.Redirect(302, down_url)
			return
		}

		response, skip := ProxyPlaybackInfo(c, proxy)
		if !skip {
			c.JSON(http.StatusOK, response)
			return
		}
		currentURI := c.Request.RequestURI
		userAgent = strings.ToLower(userAgent)
		cacheKey := RemoveQueryParams(currentURI) + userAgent

		if cacheLink, found := goCache.Get(cacheKey); found {
			logrus.Infoln("命中缓存")
			c.Redirect(302, cacheLink.(string))
			return
		}
		logrus.Infoln(currentURI)
		re = regexp.MustCompile(`/[Vv]ideos/(\S+)/(stream|original|master)`)
		matches = re.FindStringSubmatch(currentURI)
		videoID := ""
		if len(matches) >= 2 {
			videoID = matches[1]
		} else {
			proxy.ServeHTTP(c.Writer, c.Request)
			return
		}
		videoID, err := extractIDFromPath(currentURI)
		if err != nil {
			proxy.ServeHTTP(c.Writer, c.Request)
			return
		}
		mediaSourceID := c.Query("MediaSourceId")
		if mediaSourceID == "" {
			mediaSourceID = c.Query("mediaSourceId")
		}
		if videoID == "" || mediaSourceID == "" {
			proxy.ServeHTTP(c.Writer, c.Request)
			return
		}
		itemInfoUri, itemId, etag, mediaSourceId, apiKey := GetItemPathInfo(c)
		embyRes, err := GetEmbyItems(itemInfoUri, itemId, etag, mediaSourceId, apiKey)
		if err != nil {
			log.Error(fmt.Sprintf("获取 Emby 失败。错误信息: %v", err))
			proxy.ServeHTTP(c.Writer, c.Request)
			return
		}
		if !strings.HasPrefix(embyRes["path"].(string), viper.GetString("server.mount-path")) {
			proxy.ServeHTTP(c.Writer, c.Request)
			return
		}
		log.Info("Emby Path：" + embyRes["path"].(string))
		alistPath := strings.Replace(embyRes["path"].(string), viper.GetString("server.mount-path"), "", 1)
		alistPath = ensureLeadingSlash(alistPath)
		log.Info("115Path: " + alistPath)

		originalHeaders := make(map[string]string)
		for key, value := range c.Request.Header {
			if len(value) > 0 {
				originalHeaders[key] = value[0]
			}
		}

		scheme := c.Request.Header.Get("X-Forwarded-Proto")
		if scheme == "" {
			scheme = "http"
		}
		server_url := scheme + "://" + c.Request.Host + "/path" + alistPath
		logrus.Infoln("Server URL: " + server_url)
		url, err := GetRedirectURL(server_url, originalHeaders)
		if err != nil {
			log.Error(fmt.Sprintf("获取 Alist 地址失败。错误信息: %v", err))
			proxy.ServeHTTP(c.Writer, c.Request)
			return
		}
		goCache.Set(cacheKey, url, cache.DefaultExpiration)
		c.Redirect(http.StatusFound, url)

	})

	if err := r.Run(":9096"); err != nil {
		panic(err)
	}
}
