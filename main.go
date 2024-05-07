package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/davidbyttow/govips/v2/vips"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mattn/go-sqlite3"

	sniff "github.com/hexahigh/yapc/backend/lib/sniff"
)

var (
	port         = flag.String("p", ":8080", "port to listen on")
	cacheMaxSize = flag.Int("cm", 8000, "maximum size of the cache in MB")
	cacheType    = flag.String("cache", "in-memory", "Cache type: in-memory or sql")
	dbType       = flag.String("db:type", "sqlite", "SQL database type: sqlite or mysql")
	dbPass       = flag.String("db:pass", "", "Database password (Unused for sqlite)")
	dbUser       = flag.String("db:user", "root", "Database user (Unused for sqlite)")
	dbHost       = flag.String("db:host", "localhost:3306", "Database host (Unused for sqlite)")
	dbDb         = flag.String("db:db", "yapc", "Database name (Unused for sqlite)")
	dbFile       = flag.String("db:file", "./cache.db", "SQLite database file")
	dbConns      = flag.Int("db:conns", 10, "Maximum number of database connections")
	waitForIt    = flag.Bool("db:wait", false, "Wait for database connection")
	verbosity    = flag.Int("v", 0, "Verbosity level [0-3]")
)

type Cache struct {
	mu    sync.RWMutex
	m     map[string][]byte
	order []string // Slice to keep track of the order of insertion
	db    *sql.DB  // SQLite database connection
	Type  string   // Cache type: in-memory or sqlite
}

var logger *log.Logger
var cache Cache
var db *sql.DB

func init() {
	flag.Parse()
	logger = log.New(os.Stdout, "", log.LstdFlags)

	if *cacheType == "sql" {
		switch *dbType {
		case "sqlite":
			var err error
			db, err = sql.Open("sqlite3", *dbFile)
			if err != nil {
				log.Fatal(err)
			}
		case "mysql":
			var err error
			if *waitForIt {
				for {
					db, err = sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/%s", *dbUser, *dbPass, *dbHost, *dbDb))
					if err != nil {
						log.Printf("Failed to connect to database: %v", err)
						time.Sleep(time.Second * 5)
						continue
					}
					break
				}
			} else {
				db, err = sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/%s", *dbUser, *dbPass, *dbHost, *dbDb))
				if err != nil {
					log.Printf("Failed to connect to database: %v", err)
					os.Exit(1)
				}
			}
			db.SetConnMaxLifetime(time.Minute * 3)
			db.SetConnMaxIdleTime(time.Minute * 2)
			db.SetMaxOpenConns(*dbConns)
			db.SetMaxIdleConns(*dbConns)
		}

		cache = Cache{
			Type: "sql",
			db:   db,
		}
		// Initialize the SQLite table if it doesn't exist
		_, err := db.Exec(`CREATE TABLE IF NOT EXISTS cache (
			key TEXT PRIMARY KEY,
			value BLOB
		)`)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		cache = Cache{
			m:     make(map[string][]byte),
			order: make([]string, 0),
			Type:  "in-memory",
		}
	}
}

func main() {
	var wg sync.WaitGroup
	wg.Add(1)

	vips.PrintObjectReport("vips")

	go func() {
		vips.Startup(nil)
		defer vips.Shutdown()
		defer wg.Done()
		http.HandleFunc("/img", handleImg)
		http.ListenAndServe(*port, nil)
	}()

	go func() {
		// Delete the oldest item from the cache if it exceeds the max size
		for {
			_, size := cache.Stats()
			if size >= int64(*cacheMaxSize)*1024*1024 {
				logger.Println("Cache exceeds max size, deleting oldest item")
				cache.DeleteOldest()
			}
			time.Sleep(1 * time.Second)
		}
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
		Url         string
		Format      string
		Width       float64
		Height      float64
		Quality     int64
		Strip       bool
		Lossless    bool
		Compression int64
	}

	// Convert to integers
	width, ok := stringToFloat64(queryParams.Get("w"), 1, w)
	if !ok {
		return
	}
	height, ok := stringToFloat64(queryParams.Get("h"), 1, w)
	if !ok {
		return
	}
	quality, ok := stringToInt64(queryParams.Get("q"), -1, w)
	if !ok {
		return
	}
	compressionlevel, ok := stringToInt64(queryParams.Get("c"), -1, w)
	if !ok {
		return
	}

	var params Params
	params.Url = queryParams.Get("u")
	params.Format = queryParams.Get("f")
	params.Strip = queryParams.Get("s") == "1"
	params.Lossless = queryParams.Get("l") == "1"
	params.Width = width
	params.Height = height
	params.Quality = quality
	params.Compression = compressionlevel

	Vprintln(3, "Parameters: ", params)

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
	cacheKey := fmt.Sprintf("%s-%s-%d-%d-%d-%t-%d", params.Url, params.Format, params.Width, params.Height, params.Quality, params.Strip, params.Compression, params.Lossless)

	// Check if the image is in the cache
	if cachedImg, ok := cache.Get(cacheKey); ok {
		// Serve the cached image
		w.Header().Set("Content-Type", sniff.DetectContentType(cachedImg))
		w.Write(cachedImg)
		return
	}

	Vprintln(2, "Loading image")
	image, err := vips.NewImageFromBuffer(bodyBytes)
	if err != nil {
		http.Error(w, "Failed to load image", http.StatusInternalServerError)
		logger.Println(err)
		return
	}
	defer image.Close()

	kernel := vips.KernelAuto

	if params.Width != 0 || params.Height != 0 {
		Vprintln(2, "Resizing image")
		image.ResizeWithVScale(float64(params.Width), float64(params.Height), kernel)
	}

	var buf []byte

	// Gradually build the command based on the format
	switch params.Format {
	case "jpg", "jpeg":
		Vprintln(2, "Output image is JPEG")
		jpegParams := vips.JpegExportParams{
			Quality:       int(params.Quality),
			StripMetadata: params.Strip,
		}
		buf, _, err = image.ExportJpeg(&jpegParams)
		if err != nil {
			http.Error(w, "Failed to export image", http.StatusInternalServerError)
			logger.Println(err)
			return
		}
	case "png":
		Vprintln(2, "Output image is PNG")
		pngParams := vips.PngExportParams{
			Quality:       int(params.Quality),
			StripMetadata: params.Strip,
			Compression:   int(params.Compression),
		}
		buf, _, err = image.ExportPng(&pngParams)
		if err != nil {
			http.Error(w, "Failed to export image", http.StatusInternalServerError)
			logger.Println(err)
			return
		}
	case "webp":
		Vprintln(2, "Output image is WEBP")
		webpParams := vips.NewWebpExportParams()
		if quality != -1 {
			webpParams.Quality = int(params.Quality)
		}
		webpParams.StripMetadata = params.Strip
		webpParams.Lossless = params.Lossless
		if params.Compression != -1 {
			webpParams.ReductionEffort = int(params.Compression)
		}

		buf, _, err = image.ExportWebp(webpParams)
		if err != nil {
			http.Error(w, "Failed to export image", http.StatusInternalServerError)
			logger.Println(err)
			return
		}
	case "avif":
		Vprintln(2, "Output image is AVIF")
		avifParams := vips.NewAvifExportParams()
		avifParams.StripMetadata = params.Strip
		avifParams.Lossless = params.Lossless
		if params.Compression != -1 {
			avifParams.Effort = int(params.Compression)
		}
		if params.Quality != -1 {
			avifParams.Quality = int(params.Quality)
		}

		buf, _, err = image.ExportAvif(avifParams)
		if err != nil {
			http.Error(w, "Failed to export image", http.StatusInternalServerError)
			logger.Println(err)
			return
		}
	case "jxl":
		Vprintln(2, "Output image is JXL")
		jxlParams := vips.NewJxlExportParams()
		jxlParams.Lossless = params.Lossless
		if params.Compression != -1 {
			jxlParams.Effort = int(params.Compression)
		}
		if params.Quality != -1 {
			jxlParams.Quality = int(params.Quality)
		}
		buf, _, err = image.ExportJxl(jxlParams)
		if err != nil {
			http.Error(w, "Failed to export image", http.StatusInternalServerError)
			logger.Println(err)
			return
		}
	default:
		http.Error(w, "Unsupported new format", http.StatusBadRequest)
		return
	}

	// Store the processed image in the cache
	Vprintln(2, "Storing processed image in cache")
	cache.Set(cacheKey, buf)

	// Serve the processed image
	Vprintln(2, "Serving processed image")
	w.Header().Set("Content-Type", sniff.DetectContentType(buf))
	w.Write(buf)
}

func corsShit(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
}

// Set method for SQLite cache
func (c *Cache) Set(key string, value []byte) {
	if c.Type == "sql" {
		_, err := c.db.Exec("INSERT OR REPLACE INTO cache (key, value) VALUES (?, ?)", key, value)
		if err != nil {
			log.Println("Failed to set cache:", err)
		}
	} else {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.m[key] = value
		c.order = append(c.order, key) // Add the key to the order slice
	}
}

// Get method for SQLite cache
func (c *Cache) Get(key string) ([]byte, bool) {
	if c.Type == "sql" {
		var value []byte
		err := c.db.QueryRow("SELECT value FROM cache WHERE key = ?", key).Scan(&value)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil, false // Key not found
			}
			log.Println("Failed to get cache:", err)
			return nil, false
		}
		return value, true
	} else {
		c.mu.RLock()
		defer c.mu.RUnlock()
		val, ok := c.m[key]
		if ok {
			// Move the key to the end of the order slice to mark it as recently used
			for i, k := range c.order {
				if k == key {
					c.order = append(c.order[:i], c.order[i+1:]...)
					break
				}
			}
			c.order = append(c.order, key)
		}
		return val, ok
	}
}

// DeleteOldest method for SQLite cache
func (c *Cache) DeleteOldest() {
	if c.Type == "sql" {
		// TODO: Implement
	} else {
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.order) == 0 {
			return // No items to delete
		}
		oldestKey := c.order[0] // The first item in the order slice is the oldest
		delete(c.m, oldestKey)  // Remove the item from the map
		c.order = c.order[1:]   // Remove the oldest key from the order slice
	}
}

func (c *Cache) Stats() (int, int64) {
	var count int
	var totalSize int64
	if c.Type == "sql" {
		c.db.QueryRow("SELECT COUNT(*), SUM(LENGTH(value)) FROM cache").Scan(&count, &totalSize)
	} else {

		c.mu.RLock()
		defer c.mu.RUnlock()

		for _, value := range c.m {
			count++
			totalSize += int64(len(value))
		}
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

func stringToInt64(s string, def int64, w http.ResponseWriter) (int64, bool) {
	if s == "" {
		return def, true
	}
	value, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		if err.(*strconv.NumError).Err == strconv.ErrSyntax {
			http.Error(w, "Invalid parameter", http.StatusBadRequest)
			return 0, false
		}
	}
	return value, true
}

func stringToFloat64(s string, def float64, w http.ResponseWriter) (float64, bool) {
	if s == "" {
		return def, true
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if err.(*strconv.NumError).Err == strconv.ErrSyntax {
			http.Error(w, "Invalid parameter", http.StatusBadRequest)
			return 0, false
		}
	}
	return value, true
}

func Vprintln(l int, s ...any) {
	if l <= *verbosity {
		logger.Println(s)
	}
}
