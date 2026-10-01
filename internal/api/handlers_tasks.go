package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/cron-job-manager/internal/service"
)

type taskRequest struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Action   string `json:"action"`
}

type validateScheduleRequest struct {
	Schedule string `json:"schedule"`
}

func (h *handler) validateSchedule(c *gin.Context) {
	var request validateScheduleRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request_body", "request body must be valid JSON")
		return
	}
	info, err := h.service.ValidateSchedule(request.Schedule)
	if err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedule": info})
}

func (h *handler) createTask(c *gin.Context) {
	var request taskRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request_body", "request body must be valid JSON")
		return
	}
	task, err := h.service.CreateTask(service.TaskInput{
		ID:       request.ID,
		Name:     request.Name,
		Schedule: request.Schedule,
		Action:   request.Action,
	})
	if err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"task": task})
}

func (h *handler) updateTask(c *gin.Context) {
	var request taskRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request_body", "request body must be valid JSON")
		return
	}
	task, err := h.service.UpdateTask(c.Param("id"), service.TaskInput{
		Name:     request.Name,
		Schedule: request.Schedule,
		Action:   request.Action,
	})
	if err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": task})
}

func (h *handler) deleteTask(c *gin.Context) {
	if err := h.service.DeleteTask(c.Param("id")); err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": c.Param("id")})
}

func (h *handler) getTask(c *gin.Context) {
	task, err := h.service.GetTask(c.Param("id"))
	if err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": task})
}

func (h *handler) listTasks(c *gin.Context) {
	tasks, err := h.service.ListTasks()
	if err != nil {
		mapServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}
