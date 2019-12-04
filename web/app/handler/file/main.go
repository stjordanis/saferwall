// Copyright 2019 Saferwall. All rights reserved.
// Use of this source code is governed by Apache v2 license
// license that can be found in the LICENSE file.

package file

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/labstack/echo/v4"
	"github.com/minio/minio-go/v6"
	"github.com/saferwall/saferwall/pkg/crypto"
	"github.com/saferwall/saferwall/web/app"
	"github.com/saferwall/saferwall/web/app/common/db"
	"github.com/saferwall/saferwall/web/app/common/utils"
	u "github.com/saferwall/saferwall/pkg/utils"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/xeipuuv/gojsonschema"
	"gopkg.in/couchbase/gocb.v1"
)

type stringStruct struct {
	Encoding string `json:"encoding"`
	Value    string `json:"value"`
}

// File represent a sample
type File struct {
	Md5       string                 `json:"md5,omitempty"`
	Sha1      string                 `json:"sha1,omitempty"`
	Sha256    string                 `json:"sha256,omitempty"`
	Sha512    string                 `json:"sha512,omitempty"`
	Ssdeep    string                 `json:"ssdeep,omitempty"`
	Crc32     string                 `json:"crc32,omitempty"`
	Magic     string                 `json:"magic,omitempty"`
	Size      int64                  `json:"size,omitempty"`
	Exif      map[string]string      `json:"exif"`
	TriD      []string               `json:"trid"`
	Packer    []string               `json:"packer"`
	FirstSeen time.Time              `json:"first_seen,omitempty"`
	Strings   []stringStruct         `json:"strings"`
	MultiAV   map[string]interface{} `json:"multiav"`
	Status    int                    `json:"status"`
}

// Response JSON
type Response struct {
	Sha256      string `json:"sha256,omitempty"`
	Message     string `json:"message,omitempty"`
	Description string `json:"description,omitempty"`
	Filename    string `json:"filename,omitempty"`
}

// AV vendor
type AV struct {
	Vendor string `json:"vendor,omitempty"`
}

const (
	queued     = iota
	processing = iota
	finished   = iota
)

// Create creates a new file
func (file *File) Create() error {
	_, error := db.FilesBucket.Upsert(file.Sha256, file, 0)
	if error != nil {
		log.Fatal(error)
		return error
	}
	log.Infof("File %s added to database.", file.Sha256)
	return nil
}

// GetFileBySHA256 return user document
func GetFileBySHA256(sha256 string) (File, error) {

	// get our file
	file := File{}
	cas, err := db.FilesBucket.Get(sha256, &file)
	if err != nil {
		log.Errorln(err, cas)
		return file, err
	}

	return file, err
}

// GetAllFiles return all files (optional: selecting fields)
func GetAllFiles(fields []string) ([]File, error) {

	// Select only demanded fields
	var statement string
	if len(fields) > 0 {
		var buffer bytes.Buffer
		buffer.WriteString("SELECT ")
		length := len(fields)
		for index, field := range fields {
			buffer.WriteString(field)
			if index < length-1 {
				buffer.WriteString(",")
			}
		}
		buffer.WriteString(" FROM `files`")
		statement = buffer.String()
	} else {
		statement = "SELECT files.* FROM `files`"
	}

	// Execute our query
	query := gocb.NewN1qlQuery(statement)
	rows, err := db.UsersBucket.ExecuteN1qlQuery(query, nil)
	if err != nil {
		fmt.Println("Error executing n1ql query:", err)
	}

	// Interfaces for handling streaming return values
	var row File
	var retValues []File

	// Stream the values returned from the query into a typed array of structs
	for rows.Next(&row) {
		retValues = append(retValues, row)
	}

	return retValues, nil
}

//=================== /file/sha256 handlers ===================

// GetFile returns file informations.
func GetFile(c echo.Context) error {

	// get path param
	sha256 := c.Param("sha256")
	file, err := GetFileBySHA256(sha256)
	if err != nil {
		return c.JSON(http.StatusNotFound,  Response{
			Message:     err.Error(),
			Description: "File not found",
			Sha256:      sha256,
		})
	}
	return c.JSON(http.StatusOK, file)
}

// PutFile updates a specific file
func PutFile(c echo.Context) error {

	// get path param
	sha256 := c.Param("sha256")

	// Read the json body
	b, err := ioutil.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, err.Error())
	}

	// Validate JSON
	l := gojsonschema.NewBytesLoader(b)
	result, err := app.FileSchema.Validate(l)
	if err != nil {
		return c.JSON(http.StatusBadRequest, err.Error())
	}

	if !result.Valid() {
		for _, desc := range result.Errors() {
			log.Printf("- %s\n", desc)
		}
		return c.JSON(http.StatusBadRequest, errors.New("json validation failed"))
	}

	// Updates the document.
	file, err := GetFileBySHA256(sha256)
	if err != nil {
		return c.JSON(http.StatusBadRequest, err.Error())
	}
	err = json.Unmarshal(b, &file)
	if err != nil {
		return c.JSON(http.StatusBadRequest, err.Error())
	}

	db.FilesBucket.Upsert(sha256, file, 0)
	return c.JSON(http.StatusOK, sha256)
}

// DeleteFile deletes a specific file
func DeleteFile(c echo.Context) error {

	// get path param
	sha256 := c.Param("sha256")
	return c.JSON(http.StatusOK, sha256)
}

// deleteAllFiles will empty files bucket
func deleteAllFiles() {
	// Keep in mind that you must have flushing enabled in the buckets configuration.

	username := viper.GetString("db.username")
	password := viper.GetString("db.password")

	db.FilesBucket.Manager(username, password).Flush()
}

// GetFiles returns list of files.
func GetFiles(c echo.Context) error {
	// get query param `fields` for filtering & sanitize them
	filters := utils.GetQueryParamsFields(c)
	if len(filters) > 0 {
		file := File{}
		allowed := utils.IsFilterAllowed(utils.GetStructFields(file), filters)
		if !allowed {
			return c.JSON(http.StatusBadRequest, "Filters not allowed")
		}
	}

	// get all users
	allFiles, err := GetAllFiles(filters)
	if err != nil {
		return c.JSON(http.StatusBadRequest, err.Error())
	}
	return c.JSON(http.StatusOK, allFiles)
}

// PostFiles creates a new file
func PostFiles(c echo.Context) error {

	user := c.Get("user").(*jwt.Token)
	claims := user.Claims.(jwt.MapClaims)
	name := claims["name"].(string)
	log.Infoln("New file uploaded by", name)
	// Source
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, Response{
			Message:     "Missing file",
			Description: "Did you send the file via the form request ?",
		})
	}

	// Check file size
	if fileHeader.Size > app.MaxFileSize {
		return c.JSON(http.StatusRequestEntityTooLarge, Response{
			Message:     "File too large",
			Description: "The maximum allowed is 64MB",
			Filename:    fileHeader.Filename,
		})
	}

	// Open the file
	file, err := fileHeader.Open()
	if err != nil {
		log.Error("Opening a file handle failed, err: ", err)
		return c.JSON(http.StatusInternalServerError, Response{
			Message:     "Internal error",
			Description: "Internal error",
			Filename:    fileHeader.Filename,
		})
	}
	defer file.Close()

	// Get the size
	size := fileHeader.Size
	log.Infoln("File size: ", size)

	// Read the content
	fileContents, err := ioutil.ReadAll(file)
	if err != nil {
		log.Error("Opening a reading the file content, err: ", err)
		return c.JSON(http.StatusInternalServerError, Response{
			Message:     "ReadAll failed",
			Description: "Internal error",
			Filename:    fileHeader.Filename,
		})
	}

	sha256 := crypto.GetSha256(fileContents)
	log.Infoln("File hash: ", sha256)

	// Upload the sample to DO object storage.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := app.MinioClient.PutObjectWithContext(ctx, app.SamplesSpaceBucket,
		sha256, bytes.NewReader(fileContents), size,
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		log.Error("Failed to upload object, err: ", err)
		return c.JSON(http.StatusInternalServerError, Response{
			Message:     "PutObject failed",
			Description: err.Error(),
			Filename:    fileHeader.Filename,
			Sha256:      sha256,
		})
	}
	log.Println("Successfully uploaded bytes: ", n)

	// Save to DB
	NewFile := File{
		Sha256:    sha256,
		FirstSeen: time.Now().UTC(),
		Size:      fileHeader.Size,
		Status:    queued,
	}
	NewFile.Create()

	// Push it to NSQ
	err = app.NsqProducer.Publish("scan", []byte(sha256))
	if err != nil {
		log.Error("Failed to publish to NSQ, err: ", err)
		return c.JSON(http.StatusInternalServerError, Response{
			Message:     "Publish failed",
			Description: "Internal error",
			Filename:    fileHeader.Filename,
			Sha256:      sha256,
		})
	}

	// All went fine
	return c.JSON(http.StatusCreated, Response{
		Sha256:      sha256,
		Message:     "ok",
		Description: "File queued successfully for analysis",
		Filename:    fileHeader.Filename,
	})
}

// PutFiles bulk updates of files
func PutFiles(c echo.Context) error {
	return c.String(http.StatusOK, "putFiles")
}

// DeleteFiles delete all files
func DeleteFiles(c echo.Context) error {

	go deleteAllFiles()
	return c.JSON(http.StatusOK, map[string]string{
		"verbose_msg": "ok"})
}

// Download downloads a file.
func Download (c echo.Context) error {
	// get path param
	sha256 := c.Param("sha256")

	reader, err := app.MinioClient.GetObject(
		app.SamplesSpaceBucket, sha256, minio.GetObjectOptions{})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, err.Error())
	}
	defer reader.Close()

	_, err = reader.Stat()
	if err != nil {
		return c.JSON(http.StatusNotFound, err.Error())
	}

	filepath, err := u.ZipEncrypt(sha256, "infected", reader )
	if err != nil {
		return c.JSON(http.StatusBadRequest, err.Error())
	}
	return c.File(filepath)
}