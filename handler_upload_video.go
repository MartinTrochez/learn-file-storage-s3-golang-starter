package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Could not find the video", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You can't access this video", nil)
		return
	}

	const maxMemory = 10 << 30
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Could not parse form", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Could not get file", err)
		return
	}
	defer file.Close()

	mediaType := header.Header.Get("Content-Type")
	mimeType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting mimeType", err)
		return
	}

	if mimeType != "video/mp4" {
		respondWithError(w, http.StatusForbidden, "Only mp4 files accepted", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubeley-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error in the server", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error writing file", err)
		return
	}

	processedPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process video", err)
		return
	}
	defer os.Remove(processedPath)

	processedFile, err := os.Open(processedPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to open processed video", err)
		return
	}
	defer processedFile.Close()

	ext := filepath.Ext(header.Filename)

	key := make([]byte, 32)
	rand.Read(key)

	randomKey := base64.RawURLEncoding.EncodeToString(key)

	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Could no inspect video", err)
		return
	}

	aspectRatioKey := "other"

	if aspectRatio == "16:9" {
		aspectRatioKey = "landscape"
	} else if aspectRatio == "9:16" {
		aspectRatioKey = "portrait"
	}

	fileKey := aspectRatioKey + "/" + randomKey + ext

	params := &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		ContentType: &mimeType,
		Body:        processedFile,
	}

	_, err = cfg.s3Client.PutObject(r.Context(), params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload video", err)
		return
	}

	videoURL := "https://" + cfg.s3CfDistribution + "/" + fileKey
	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not update the video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath,
	)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", err
	}

	type ffprobeResult struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	var unmarshalResult ffprobeResult
	if err := json.Unmarshal(stdout.Bytes(), &unmarshalResult); err != nil {
		return "", err
	}

	if len(unmarshalResult.Streams) == 0 {
		return "", fmt.Errorf("No streams found")
	}

	width := unmarshalResult.Streams[0].Width
	height := unmarshalResult.Streams[0].Height

	if width == 0 || height == 0 {
		return "", fmt.Errorf("Invalid video dimensions")
	}

	ratio := float64(width) / float64(height)

	aspect := "other"
	tolerance := 0.01

	if math.Abs(ratio-(16.0/9.0)) < tolerance {
		aspect = "16:9"

	} else if math.Abs(ratio-(9.0/16.0)) < tolerance {
		aspect = "9:16"
	}

	return aspect, nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outPath := filePath + ".processing"

	cmd := exec.Command(
		"ffmpeg", "-i", filePath, "-c", "copy",
		"-movflags", "faststart", "-f", "mp4", outPath,
	)

	if err := cmd.Run(); err != nil {
		return "", err
	}
	return outPath, nil
}
