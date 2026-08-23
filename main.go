package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

const port = ":8080"
const targetURL = "https://youtu.be/yWJavgEeHGs?si=B3bRWVKzWBmC_d6l"

func redirectAndLogHandler(w http.ResponseWriter, r *http.Request) {
	currentTime := time.Now().Format("2006-01-02 15:04:05")

	ip := r.Header.Get("CF-Connecting-IP")
	if ip == "" {
		ip = r.Header.Get("X-Forwarded-For")
	}
	if ip == "" {
		ip = r.RemoteAddr
	}

	country := r.Header.Get("CF-IPCountry")
	if country == "" {
		country = "Unknown"
	}

	userAgent := r.UserAgent()
	method := r.Method
	path := r.URL.Path

	logLine := fmt.Sprintf("[%s] IP: %s | Country: %s | Method: %s | Path: %s | User-Agent: %s\n",
		currentTime, ip, country, method, path, userAgent)

	fmt.Print(logLine)

	f, err := os.OpenFile("/app/data/qr_access.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		f.WriteString(logLine)
		f.Close()
	} else {
		log.Println("Error writing to log file:", err)
	}

	http.Redirect(w, r, targetURL, http.StatusFound)
}

func main() {
	http.HandleFunc("/", redirectAndLogHandler)

	fmt.Printf("Server is listening on %s and redirecting to %s\n", port, targetURL)

	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
