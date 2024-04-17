package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	sniff "github.com/hexahigh/yapc/backend/lib/sniff"
)

var (
	port = flag.String("p", ":8080", "port to listen on")
)

var logger *log.Logger

func init() {
	flag.Parse()
	logger = log.New(os.Stdout, "", log.LstdFlags)
}

func main() {

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		http.HandleFunc("/img", handleImg)
		http.ListenAndServe(*port, nil)
	}()

	go func() {
		// Print cache stats every 10 seconds
		for {
			count, size := cache.Stats()
			logger.Printf("Cache stats: %d images, %s", count, humanBytes(size))
			time.Sleep(10 * time.Second)
		}

	}()

	wg.Wait()

}

func handleImg(w http.ResponseWriter, r *http.Request) {
	corsShit(w)

	// Parse the URL query parameters
	queryParams, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "Failed to parse query parameters", http.StatusBadRequest)
		return
	}

	type Params struct {
		Url     string
		Format  string
		Width   int
		Height  int
		Quality int
	}

	// Convert to integers
	width, err := strconv.Atoi(queryParams.Get("w"))
	if err != nil {
		if err.(*strconv.NumError).Err == strconv.ErrSyntax && queryParams.Get("w") == "" {
			// If the error is due to an empty string, default to 0
			width = 0
		} else {
			http.Error(w, "Invalid width parameter", http.StatusBadRequest)
			return
		}
	}
	height, err := strconv.Atoi(queryParams.Get("h"))
	if err != nil {
		if err.(*strconv.NumError).Err == strconv.ErrSyntax && queryParams.Get("h") == "" {
			// If the error is due to an empty string, default to 0
			height = 0
		} else {
			http.Error(w, "Invalid height parameter", http.StatusBadRequest)
			return
		}
	}
	quality, err := strconv.Atoi(queryParams.Get("q"))
	if err != nil {
		if err.(*strconv.NumError).Err == strconv.ErrSyntax && queryParams.Get("q") == "" {
			// If the error is due to an empty string, default to 0
			quality = 0
		} else {
			http.Error(w, "Invalid quality parameter", http.StatusBadRequest)
			return
		}
	}

	var params Params
	params.Url = queryParams.Get("u")
	params.Format = queryParams.Get("f")
	params.Width = width
	params.Height = height
	params.Quality = quality

	// Fetch the original image
	resp, err := http.Get(params.Url)
	if err != nil {
		http.Error(w, "Failed to fetch original image", http.StatusInternalServerError)
		logger.Println(err)
		return
	}
	defer resp.Body.Close()

	// Read the response body into a byte slice
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read response body", http.StatusInternalServerError)
		return
	}

	// Generate a unique cache key
	cacheKey := fmt.Sprintf("%s-%d-%d-%s", params.Url, params.Width, params.Height, params.Format)

	// Check if the image is in the cache
	if cachedImg, ok := cache.Get(cacheKey); ok {
		// Serve the cached image
		w.Header().Set("Content-Type", sniff.DetectContentType(cachedImg))
		w.Write(cachedImg)
		return
	}

	var command string

	command = "ffmpeg -i -"

	if params.Quality != 0 {
		command += " -q:v " + strconv.Itoa(params.Quality)
	}

	if params.Width != 0 || params.Height != 0 {
		command = fmt.Sprintf("%s -vf scale=%d:%d", command, params.Width, params.Height)
	}

	// Gradually build the command based on the format
	switch params.Format {
	case "jpg", "jpeg":
		command += " -c:v mjpeg -f image2pipe -"
	case "png":
		command += " -c:v png -f image2pipe -"
	case "gif":
		command += " -f gif -"
	case "webp":
		command += " -f webp -"
	default:
		http.Error(w, "Unsupported new format", http.StatusBadRequest)
		return
	}

	// Execute the command
	logger.Println("Running command:", command)
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = bytes.NewReader(bodyBytes)
	var buf bytes.Buffer
	cmd.Stdout = &buf

	// Capture stderr
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		http.Error(w, "Failed to convert image", http.StatusInternalServerError)
		logger.Println("Command execution failed:", err)
		logger.Println("ffmpeg error output:", stderr.String())
		return
	}

	// Store the processed image in the cache
	cache.Set(cacheKey, buf.Bytes())

	// Serve the processed image
	w.Header().Set("Content-Type", sniff.DetectContentType(buf.Bytes()))
	w.Write(buf.Bytes())
}

func corsShit(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
}

// Define a cache structure
type Cache struct {
	mu sync.RWMutex
	m  map[string][]byte
}

// Initialize the cache
var cache = Cache{
	m: make(map[string][]byte),
}

func (c *Cache) Set(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = value
}

func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	val, ok := c.m[key]
	return val, ok
}

func (c *Cache) Stats() (int, int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var count int
	var totalSize int64
	for _, value := range c.m {
		count++
		totalSize += int64(len(value))
	}

	return count, totalSize
}

func humanBytes(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}
