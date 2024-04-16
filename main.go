package main

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"flag"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"

	"bytes"

	sniff "github.com/hexahigh/yapc/backend/lib/sniff"
	"github.com/nfnt/resize"
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
	http.HandleFunc("/img", handleImg)
	http.ListenAndServe(*port, nil)
}

func handleImg(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "Invalid width parameter", http.StatusBadRequest)
		return
	}
	height, err := strconv.Atoi(queryParams.Get("h"))
	if err != nil {
		http.Error(w, "Invalid height parameter", http.StatusBadRequest)
		return
	}
	quality, err := strconv.Atoi(queryParams.Get("q"))
	if err != nil {
		if err.(*strconv.NumError).Err == strconv.ErrSyntax && queryParams.Get("q") == "" {
			// If the error is due to an empty string, default to 100
			quality = 100
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

	// Get content type
	contentType := sniff.DetectContentType(bodyBytes)

	var img image.Image

	switch contentType {
	case "image/jpeg":
		img, err = jpeg.Decode(bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "Failed to decode JPEG image", http.StatusInternalServerError)
			return
		}
	case "image/png":
		img, err = png.Decode(bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "Failed to decode PNG image", http.StatusInternalServerError)
			return
		}
	case "image/gif":
		img, err = gif.Decode(bytes.NewReader(bodyBytes))
		if err != nil {
			http.Error(w, "Failed to decode GIF image", http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "Unsupported image format", http.StatusBadRequest)
		return
	}

	img = resize.Resize(uint(params.Width), uint(params.Height), img, resize.Bilinear)

	switch params.Format {
	case "jpg":
		options := &jpeg.Options{
			Quality: quality,
		}
		err := jpeg.Encode(w, img, options)
		if err != nil {
			http.Error(w, "Failed to encode JPEG image", http.StatusInternalServerError)
			return
		}
	case "png":
		err := png.Encode(w, img)
		if err != nil {
			http.Error(w, "Failed to encode PNG image", http.StatusInternalServerError)
			return
		}
	case "gif":
		err := gif.Encode(w, img, nil)
		if err != nil {
			http.Error(w, "Failed to encode GIF image", http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "Unsupported new format", http.StatusBadRequest)
		return
	}
}
