package controller

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"one-api/common"
	"os"
	"strconv"
	"strings"
	"time"
)

func openaiStreamHandler(c *gin.Context, resp *http.Response, relayMode int) (*OpenAIErrorWithStatusCode, string) {
	responseText := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := strings.Index(string(data), "\n"); i >= 0 {
			return i + 1, data[0:i], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	dataChan := make(chan string)
	stopChan := make(chan bool)
	go func() {
		for scanner.Scan() {
			data := scanner.Text()
			if len(data) < 6 { // ignore blank line or wrong format
				continue
			}
			if data[:6] != "data: " && data[:6] != "[DONE]" {
				continue
			}

			// 控制流传输的速度
			speed := 15 * time.Millisecond
			speedEnv := os.Getenv("STREAM_SPEED")
			if speedEnv != "" {
				atoi, err := strconv.Atoi(speedEnv)
				if err == nil {
					speed = time.Duration(atoi) * time.Millisecond
				}
			}
			isOK := make(chan struct{})
			go controlStreamSpeed(dataChan, data, isOK, speed)
			<-isOK
			// dataChan <- data
			data = data[6:]
			if !strings.HasPrefix(data, "[DONE]") {
				switch relayMode {
				case RelayModeChatCompletions:
					var streamResponse ChatCompletionsStreamResponse
					err := json.Unmarshal([]byte(data), &streamResponse)
					if err != nil {
						common.SysError("error unmarshalling stream response: " + err.Error())
						continue // just ignore the error
					}
					for _, choice := range streamResponse.Choices {
						responseText += choice.Delta.Content
					}
				case RelayModeCompletions:
					var streamResponse CompletionsStreamResponse
					err := json.Unmarshal([]byte(data), &streamResponse)
					if err != nil {
						common.SysError("error unmarshalling stream response: " + err.Error())
						continue
					}
					for _, choice := range streamResponse.Choices {
						responseText += choice.Text
					}
				}
			}
		}
		stopChan <- true
	}()
	setEventStreamHeaders(c)
	c.Stream(func(w io.Writer) bool {
		select {
		case data := <-dataChan:
			if strings.HasPrefix(data, "data: [DONE]") {
				data = data[:12]
			}
			// some implementations may add \r at the end of data
			data = strings.TrimSuffix(data, "\r")
			c.Render(-1, common.CustomEvent{Data: data})
			return true
		case <-stopChan:
			return false
		}
	})
	err := resp.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), ""
	}
	return nil, responseText
}

type TR struct {
	Id      string `json:"id"`
	Object  string `json:"object"`
	Created int    `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		FinishReason interface{} `json:"finish_reason"`
		Delta        struct {
			Content string `json:"content"`
		} `json:"delta"`
		ContentFilterResults struct {
			Hate struct {
				Filtered bool   `json:"filtered"`
				Severity string `json:"severity"`
			} `json:"hate"`
			SelfHarm struct {
				Filtered bool   `json:"filtered"`
				Severity string `json:"severity"`
			} `json:"self_harm"`
			Sexual struct {
				Filtered bool   `json:"filtered"`
				Severity string `json:"severity"`
			} `json:"sexual"`
			Violence struct {
				Filtered bool   `json:"filtered"`
				Severity string `json:"severity"`
			} `json:"violence"`
		} `json:"content_filter_results"`
	} `json:"choices"`
	Usage interface{} `json:"usage"`
}

func controlStreamSpeed(dataChan chan string, data string, isOK chan struct{}, speed time.Duration) {
	j := data[6:]
	d := data[:6]
	var src, dst TR
	err := json.Unmarshal([]byte(j), &src)
	if err != nil {
		fmt.Println("json err:", data)
		dataChan <- data
		isOK <- struct{}{}
		return
	}
	if len(src.Choices) == 0 {
		fmt.Println("choices err", src.Choices)
		dataChan <- data
		isOK <- struct{}{}
		return
	}
	if len(src.Choices[0].Delta.Content) == 0 {
		fmt.Println("choices empty", src.Choices)
		dataChan <- data
		isOK <- struct{}{}
		return
	}
	dst = src
	for _, word := range strings.Split(src.Choices[0].Delta.Content, "") {
		dst.Choices[0].Delta.Content = word
		out, err := json.Marshal(dst)
		if err != nil {
			fmt.Println("gg")
			isOK <- struct{}{}
			return
		}
		time.Sleep(speed)
		dataChan <- d + string(out)
	}
	isOK <- struct{}{}
}

func openaiHandler(c *gin.Context, resp *http.Response, consumeQuota bool, promptTokens int, model string) (*OpenAIErrorWithStatusCode, *Usage) {
	var textResponse TextResponse
	if consumeQuota {
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return errorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
		}
		err = resp.Body.Close()
		if err != nil {
			return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
		}
		err = json.Unmarshal(responseBody, &textResponse)
		if err != nil {
			return errorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil
		}
		if textResponse.Error.Type != "" {
			return &OpenAIErrorWithStatusCode{
				OpenAIError: textResponse.Error,
				StatusCode:  resp.StatusCode,
			}, nil
		}
		// Reset response body
		resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))
	}
	// We shouldn't set the header before we parse the response body, because the parse part may fail.
	// And then we will have to send an error response, but in this case, the header has already been set.
	// So the httpClient will be confused by the response.
	// For example, Postman will report error, and we cannot check the response at all.
	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, err := io.Copy(c.Writer, resp.Body)
	if err != nil {
		return errorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}

	if textResponse.Usage.TotalTokens == 0 {
		completionTokens := 0
		for _, choice := range textResponse.Choices {
			completionTokens += countTokenText(choice.Message.Content, model)
		}
		textResponse.Usage = Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		}
	}
	return nil, &textResponse.Usage
}
