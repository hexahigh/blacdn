package main

import (
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

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

	go func() {
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

	command = "ffmpeg -loglevel warning -i -"

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
	case "avif":
		// Avif does not support outputting to a pipe so we need to do this unholy mess
		tmpFile := os.TempDir() + "/" + strconv.Itoa(rand.Int())
		command += fmt.Sprintf(" -f avif %s && cat %s && rm %s", tmpFile, tmpFile, tmpFile)
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
