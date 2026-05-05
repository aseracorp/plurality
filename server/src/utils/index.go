package utils

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"os"
)

var AlphaNumRunes = []rune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func GenerateRandomString(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = AlphaNumRunes[rand.Intn(len(AlphaNumRunes))]
	}
	return string(b)
}

func SendHTTPError(w http.ResponseWriter, message string, code int) {
	if os.Getenv("LOG_LEVEL") == "DEBUG" {
		Error("User error", nil, message)
		http.Error(w, message, code)
	} else {
		userError := GenerateRandomString(8)
		Error("User error", nil, userError, ":", message)
		http.Error(w, "An unexpected error happened (Code: "+userError+") \n Try refreshing the page / update the app. \n You can also try re-selecting your tools/models.", http.StatusInternalServerError)
	}
}

func SPAHandler(targetFolder string) http.Handler {
	fs := http.FileServer(http.Dir(targetFolder))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Debug("Serving SPA from %s", targetFolder+r.URL.Path)
		if stat, err := os.Stat(targetFolder + r.URL.Path); os.IsNotExist(err) || stat.IsDir() {
			Debug("Serving SPA index.html")
			http.ServeFile(w, r, targetFolder+"/index.html")
		} else {
			Debug("Serving SPA from %s", targetFolder+r.URL.Path)
			fs.ServeHTTP(w, r)
		}
	})
}

func ParseJson(jsonStr string) map[string]interface{} {
	var result map[string]interface{}
	json.Unmarshal([]byte(jsonStr), &result)
	return result
}

func ParseJsonString(jsonStr string) map[string]string {
	var result map[string]string
	json.Unmarshal([]byte(jsonStr), &result)
	return result
}

func ContainsString(arr []string, str string) bool {
	for _, a := range arr {
		if a == str {
			return true
		}
	}
	return false
}

func ByteSliceToIntSlice(byteSlice []byte) []int {
	intSlice := make([]int, len(byteSlice))
	for i, b := range byteSlice {
		intSlice[i] = int(b)
	}
	return intSlice
}
