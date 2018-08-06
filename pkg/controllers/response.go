package controllers

import (
	"encoding/xml"

	"github.com/gin-gonic/gin"
	"github.com/minio/minio/cmd"
)

type CreateQueueResponse struct {
	XMLName   xml.Name `xml:"CreateQueueResponse"`
	QueueURL  string   `xml:"CreateQueueResult>QueueUrl"`
	RequestID string   `xml:"ResponseMetadata>RequestId"`
}

func writeErrorResponse(c *gin.Context, errorCode cmd.APIErrorCode) {
	apiError := cmd.GetAPIError(errorCode)
	errorResponse := cmd.GetAPIErrorResponse(apiError, c.Request.URL.Path)
	c.XML(apiError.HTTPStatusCode, errorResponse)
}