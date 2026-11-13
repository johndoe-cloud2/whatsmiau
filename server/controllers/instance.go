package controllers

import (
	"encoding/base64"
	"errors"
	"math/rand/v2"
	"net/http"

	"github.com/verbeux-ai/whatsmiau/env"
	"github.com/verbeux-ai/whatsmiau/lib/whatsmiau"
	"github.com/verbeux-ai/whatsmiau/models"
	"github.com/verbeux-ai/whatsmiau/repositories/instances"
	"go.mau.fi/whatsmeow/types"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v4"
	"github.com/skip2/go-qrcode"
	"github.com/verbeux-ai/whatsmiau/interfaces"
	"github.com/verbeux-ai/whatsmiau/server/dto"
	"github.com/verbeux-ai/whatsmiau/utils"
	"go.uber.org/zap"
)

type Instance struct {
	repo      interfaces.InstanceRepository
	whatsmiau *whatsmiau.Whatsmiau
}

func NewInstances(repository interfaces.InstanceRepository, whatsmiau *whatsmiau.Whatsmiau) *Instance {
	return &Instance{
		repo:      repository,
		whatsmiau: whatsmiau,
	}
}

// instanceWithEffectiveWebhook returns a copy of the instance with Webhook.Url set from env WEBHOOK_URL
// when the instance has no per-instance webhook URL, so API responses show where events are sent.
func instanceWithEffectiveWebhook(instance *models.Instance) *models.Instance {
	if instance == nil {
		return nil
	}
	out := *instance
	out.Webhook = instance.Webhook
	if env.Env.WebhookURL != "" && out.Webhook.Url == "" {
		out.Webhook.Url = env.Env.WebhookURL
	}
	return &out
}

// Create godoc
// @Summary      Create a new WhatsApp instance
// @Description  Creates a new WhatsApp instance with the given name and optional configuration
// @Tags         Instance
// @Accept       json
// @Produce      json
// @Security     ApiKeyAuth
// @Param        body  body      dto.CreateInstanceRequest  true  "Instance creation parameters"
// @Success      201   {object}  dto.CreateInstanceResponse
// @Failure      400   {object}  utils.HTTPErrorResponse
// @Failure      422   {object}  utils.HTTPErrorResponse
// @Failure      500   {object}  utils.HTTPErrorResponse
// @Router       /v1/instance [post]
// @Router       /v1/instance/create [post]
func (s *Instance) Create(ctx echo.Context) error {
	var request dto.CreateInstanceRequest
	if err := ctx.Bind(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusUnprocessableEntity, err, "failed to bind request body")
	}

	if err := validator.New().Struct(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusBadRequest, err, "invalid request body")
	}

	request.ID = request.InstanceName
	if request.Instance == nil {
		request.Instance = &models.Instance{
			ID: request.InstanceName,
		}
	} else {
		request.Instance.ID = request.InstanceName
	}
	request.RemoteJID = ""

	if err := validateWebhookHeaders(request.Instance.Webhook.Headers); err != nil {
		return utils.HTTPFail(ctx, http.StatusBadRequest, err, err.Error())
	}
	if err := validateWebhookHeaderTransport(request.Instance.Webhook.Url, request.Instance.Webhook.Headers); err != nil {
		return utils.HTTPFail(ctx, http.StatusBadRequest, err, err.Error())
	}

	if len(request.ProxyHost) <= 0 && len(env.Env.ProxyAddresses) > 0 {
		rd := rand.IntN(len(env.Env.ProxyAddresses))
		proxyUrl := env.Env.ProxyAddresses[rd]

		proxy, err := parseProxyURL(proxyUrl)
		if err != nil {
			return utils.HTTPFail(ctx, http.StatusUnprocessableEntity, err, "invalid proxy url on env")
		}
		request.InstanceProxy = *proxy
	}

	c := ctx.Request().Context()
	if err := s.repo.Create(c, request.Instance); err != nil {
		zap.L().Error("failed to create instance", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to create instance")
	}

	if env.Env.BackendPublicURL != "" {
		if redisRepo, ok := s.repo.(*instances.RedisInstance); ok {
			if err := redisRepo.SetRoute(c, request.Instance.ID, env.Env.BackendPublicURL); err != nil {
				zap.L().Warn("failed to set route in Redis", zap.Error(err), zap.String("instance", request.Instance.ID))
			}
		}
	}

	// If migration data is present, import the Baileys session
	if request.Migration != nil {
		result, err := s.whatsmiau.Migrate(c, request.InstanceName, request.Migration.Creds, request.Migration.PreKeys)
		if err != nil {
			zap.L().Error("failed to migrate instance", zap.Error(err))
			return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to migrate instance")
		}

		return ctx.JSON(http.StatusCreated, dto.CreateInstanceResponse{
			Instance: request.Instance,
			Migration: &dto.MigrationResult{
				JID:       result.JID,
				LID:       result.LID,
				PreKeys:   result.PreKeys,
				Connected: result.Connected,
			},
		})
	}

	return ctx.JSON(http.StatusCreated, dto.CreateInstanceResponse{
		Instance: instanceWithEffectiveWebhook(request.Instance),
	})
}

// Update godoc
// @Summary      Update an existing instance
// @Description  Updates webhook and proxy settings for the given instance
// @Tags         Instance
// @Accept       json
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id    path      string                     true  "Instance ID"
// @Param        body  body      dto.UpdateInstanceRequest   true  "Update parameters"
// @Success      201   {object}  dto.UpdateInstanceResponse
// @Failure      400   {object}  utils.HTTPErrorResponse
// @Failure      404   {object}  utils.HTTPErrorResponse
// @Failure      422   {object}  utils.HTTPErrorResponse
// @Failure      500   {object}  utils.HTTPErrorResponse
// @Router       /v1/instance/update/{id} [put]
func (s *Instance) Update(ctx echo.Context) error {
	var request dto.UpdateInstanceRequest
	if err := ctx.Bind(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusUnprocessableEntity, err, "failed to bind request body")
	}

	if err := validator.New().Struct(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusBadRequest, err, "invalid request body")
	}

	c := ctx.Request().Context()

	toUpdate := &models.Instance{
		ID: request.ID,
		Webhook: models.InstanceWebhook{
			Enabled: request.Webhook.Enabled,
			Url:     request.Webhook.URL,
			Base64:  request.Webhook.Base64,
			Events:  request.Webhook.Events,
		},
		InstanceProxy: request.InstanceProxy,
	}
	if request.GroupsIgnore != nil {
		toUpdate.GroupsIgnore = request.GroupsIgnore
	}
	if request.SaveMedia != nil {
		toUpdate.SaveMedia = request.SaveMedia
	}
	if request.ReadMessages != nil {
		toUpdate.ReadMessages = request.ReadMessages
	}
	if request.AlwaysOnline != nil {
		toUpdate.AlwaysOnline = request.AlwaysOnline
	}
	if request.MsgCall != nil {
		toUpdate.MsgCall = request.MsgCall
	}
	if request.RejectCall != nil {
		toUpdate.RejectCall = request.RejectCall
	}

	if request.Webhook.URL != "" {
		stored, err := s.repo.List(c, request.ID)
		if err != nil {
			zap.L().Error("failed to list instances", zap.Error(err))
			return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to update instance")
		}
		var storedHeaders map[string]string
		if len(stored) > 0 {
			storedHeaders = stored[0].Webhook.Headers
		}
		if err := validateWebhookHeaderTransport(request.Webhook.URL, storedHeaders); err != nil {
			return utils.HTTPFail(ctx, http.StatusBadRequest, err, err.Error())
		}
	}

	instance, err := s.repo.Update(c, request.ID, toUpdate)
	if err != nil {
		if errors.Is(err, instances.ErrorNotFound) {
			return utils.HTTPFail(ctx, http.StatusNotFound, err, "instance not found")
		}
		zap.L().Error("failed to create instance", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to update instance")
	}

	// Invalidate in-memory cache so settings like GroupsIgnore take effect immediately
	s.whatsmiau.InvalidateInstanceCache(request.ID)

	// Apply presence right away so the toggle takes effect without waiting for a reconnect
	if request.AlwaysOnline != nil {
		s.whatsmiau.ApplyPresence(request.ID, *request.AlwaysOnline)
	}

	return ctx.JSON(http.StatusCreated, dto.UpdateInstanceResponse{
		Instance: instanceWithEffectiveWebhook(instance),
	})
}

// List godoc
// @Summary      List instances
// @Description  Returns all instances, optionally filtered by name or ID
// @Tags         Instance
// @Produce      json
// @Security     ApiKeyAuth
// @Param        instanceName  query     string  false  "Filter by instance name"
// @Param        id            query     string  false  "Filter by instance ID"
// @Success      200  {array}   dto.ListInstancesResponse
// @Failure      422  {object}  utils.HTTPErrorResponse
// @Failure      500  {object}  utils.HTTPErrorResponse
// @Router       /v1/instance [get]
// @Router       /v1/instance/fetchInstances [get]
func (s *Instance) List(ctx echo.Context) error {
	c := ctx.Request().Context()
	var request dto.ListInstancesRequest
	if err := ctx.Bind(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusUnprocessableEntity, err, "failed to bind request body")
	}
	if request.InstanceName == "" {
		request.InstanceName = request.ID
	}

	result, err := s.repo.List(c, request.InstanceName)
	if err != nil {
		zap.L().Error("failed to list instances", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to list instances")
	}

	var response []dto.ListInstancesResponse
	for _, instance := range result {
		jid, err := types.ParseJID(instance.RemoteJID)
		if err != nil {
			zap.L().Error("failed to parse jid", zap.Error(err))
		}

		instance.Webhook.Headers = nil

		response = append(response, dto.ListInstancesResponse{
			Instance:     instanceWithEffectiveWebhook(&instance),
			OwnerJID:     jid.ToNonAD().String(),
			InstanceName: instance.ID,
		})
	}

	if len(response) == 0 {
		return ctx.JSON(http.StatusOK, []string{})
	}

	return ctx.JSON(http.StatusOK, response)
}

// Connect godoc
// @Summary      Connect an instance (get QR code)
// @Description  Initiates connection for an instance. Returns a base64-encoded QR code PNG if not yet connected, or a connected status message. While QR generation or a saved session reconnection is pending, returns 200 with connected=false and no base64. Saved sessions are preserved during reconnection.
// @Tags         Instance
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id  path      string  true  "Instance ID"
// @Success      200  {object}  dto.ConnectInstanceResponse
// @Failure      404  {object}  utils.HTTPErrorResponse
// @Failure      422  {object}  utils.HTTPErrorResponse
// @Failure      500  {object}  utils.HTTPErrorResponse
// @Router       /v1/instance/{id}/connect [post]
// @Router       /v1/instance/connect/{id} [get]
func (s *Instance) Connect(ctx echo.Context) error {
	c := ctx.Request().Context()
	var request dto.ConnectInstanceRequest
	if err := ctx.Bind(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusUnprocessableEntity, err, "failed to bind request body")
	}

	result, err := s.repo.List(c, request.ID)
	if err != nil {
		zap.L().Error("failed to list instances", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to list instances")
	}

	if len(result) == 0 {
		// Create instance directly so Connect works without a prior Create call
		newInstance := &models.Instance{ID: request.ID}
		if len(env.Env.ProxyAddresses) > 0 {
			rd := rand.IntN(len(env.Env.ProxyAddresses))
			proxy, err := parseProxyURL(env.Env.ProxyAddresses[rd])
			if err != nil {
				zap.L().Warn("connect: invalid proxy url on env, skipping proxy", zap.Error(err))
			} else {
				newInstance.InstanceProxy = *proxy
			}
		}
		if err := s.repo.Create(c, newInstance); err != nil {
			zap.L().Error("failed to create instance on connect", zap.Error(err), zap.String("id", request.ID))
			return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to create instance")
		}
		if env.Env.BackendPublicURL != "" {
			if redisRepo, ok := s.repo.(*instances.RedisInstance); ok {
				if err := redisRepo.SetRoute(c, newInstance.ID, env.Env.BackendPublicURL); err != nil {
					zap.L().Warn("failed to set route in Redis", zap.Error(err), zap.String("instance", newInstance.ID))
				}
			}
		}
	} else if env.Env.BackendPublicURL != "" {
		// Instance already exists (e.g. Create ran on another backend). Ensure route points to THIS backend,
		// which will have the client after Connect (multi-backend prod fix).
		if redisRepo, ok := s.repo.(*instances.RedisInstance); ok {
			if err := redisRepo.SetRoute(c, request.ID, env.Env.BackendPublicURL); err != nil {
				zap.L().Warn("failed to set route in Redis on connect", zap.Error(err), zap.String("instance", request.ID))
			}
		}
	}

	qrCode, pairingCode, pairingError, err := s.whatsmiau.Connect(c, request.ID, request.Number)
	if err != nil {
		if errors.Is(err, whatsmiau.ErrAwaitingConnection) {
			return ctx.JSON(http.StatusOK, dto.ConnectInstanceResponse{
				Connected: false,
				Message:   "waiting for saved session connection",
			})
		}
		if errors.Is(err, whatsmiau.ErrAwaitingQR) {
			return ctx.JSON(http.StatusOK, dto.ConnectInstanceResponse{
				Connected: false,
				Message:   "waiting for QR code generation",
			})
		}
		zap.L().Error("failed to connect instance", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to connect instance")
	}
	if qrCode != "" {
		png, err := qrcode.Encode(qrCode, qrcode.Medium, 512)
		if err != nil {
			zap.L().Error("failed to encode qrcode", zap.Error(err))
			return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to encode qrcode")
		}
		return ctx.JSON(http.StatusOK, dto.ConnectInstanceResponse{
			Message:      "If instance restart this instance could be lost if you cannot connect",
			Connected:    false,
			Base64:       "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
			PairingCode:  pairingCode,
			PairingError: pairingError,
		})
	}

	// No QR in this response: either already connected or still waiting (client will poll again)
	status, err := s.whatsmiau.Status(request.ID)
	if err != nil {
		zap.L().Error("failed to get status after connect", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to get status")
	}
	// Connected or Connecting (post-scan 515 reconnect): user scanned successfully, stop polling
	if status == whatsmiau.Connected || status == whatsmiau.Connecting {
		phoneNumber := s.whatsmiau.PhoneE164(c, request.ID)
		return ctx.JSON(http.StatusOK, dto.ConnectInstanceResponse{
			Message:     "instance already connected",
			Connected:   true,
			PhoneNumber: phoneNumber,
		})
	}
	return ctx.JSON(http.StatusOK, dto.ConnectInstanceResponse{
		Message:   "waiting for QR",
		Connected: false,
	})
}

// ConnectQRBuffer godoc
// @Summary      Get QR code as PNG image
// @Description  Returns the QR code as a raw PNG image buffer. Returns 204 No Content if already connected.
// @Tags         Instance
// @Produce      png
// @Security     ApiKeyAuth
// @Param        id  path      string  true  "Instance ID"
// @Success      200  {file}    binary  "QR code PNG image"
// @Success      204  "Instance already connected"
// @Failure      404  {object}  utils.HTTPErrorResponse
// @Failure      422  {object}  utils.HTTPErrorResponse
// @Failure      500  {object}  utils.HTTPErrorResponse
// @Router       /v1/instance/connect/{id}/image [get]
func (s *Instance) ConnectQRBuffer(ctx echo.Context) error {
	c := ctx.Request().Context()
	var request dto.ConnectInstanceRequest
	if err := ctx.Bind(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusUnprocessableEntity, err, "failed to bind request body")
	}

	result, err := s.repo.List(c, request.ID)
	if err != nil {
		zap.L().Error("failed to list instances", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to list instances")
	}

	if len(result) == 0 {
		newInstance := &models.Instance{ID: request.ID}
		if len(env.Env.ProxyAddresses) > 0 {
			rd := rand.IntN(len(env.Env.ProxyAddresses))
			proxy, err := parseProxyURL(env.Env.ProxyAddresses[rd])
			if err != nil {
				zap.L().Warn("connect QR buffer: invalid proxy url on env, skipping proxy", zap.Error(err))
			} else {
				newInstance.InstanceProxy = *proxy
			}
		}
		if err := s.repo.Create(c, newInstance); err != nil {
			zap.L().Error("failed to create instance on connect QR buffer", zap.Error(err), zap.String("id", request.ID))
			return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to create instance")
		}
		if env.Env.BackendPublicURL != "" {
			if redisRepo, ok := s.repo.(*instances.RedisInstance); ok {
				if err := redisRepo.SetRoute(c, newInstance.ID, env.Env.BackendPublicURL); err != nil {
					zap.L().Warn("failed to set route in Redis", zap.Error(err), zap.String("instance", newInstance.ID))
				}
			}
		}
	} else if env.Env.BackendPublicURL != "" {
		if redisRepo, ok := s.repo.(*instances.RedisInstance); ok {
			if err := redisRepo.SetRoute(c, request.ID, env.Env.BackendPublicURL); err != nil {
				zap.L().Warn("failed to set route in Redis on connect QR buffer", zap.Error(err), zap.String("instance", request.ID))
			}
		}
	}

	qrCode, _, _, err := s.whatsmiau.Connect(c, request.ID, "")
	if err != nil {
		if errors.Is(err, whatsmiau.ErrAwaitingQR) || errors.Is(err, whatsmiau.ErrAwaitingConnection) {
			return ctx.NoContent(http.StatusOK)
		}
		zap.L().Error("failed to connect instance", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to connect instance")
	}
	if qrCode != "" {
		png, err := qrcode.Encode(qrCode, qrcode.Medium, 256)
		if err != nil {
			zap.L().Error("failed to encode qrcode", zap.Error(err))
			return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to encode qrcode")
		}
		return ctx.Blob(http.StatusOK, "image/png", png)
	}

	return ctx.NoContent(http.StatusOK)
}

// Status godoc
// @Summary      Get instance connection state
// @Description  Returns the current connection state of the specified instance
// @Tags         Instance
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id  path      string  true  "Instance ID"
// @Success      200  {object}  dto.StatusInstanceResponse
// @Failure      404  {object}  utils.HTTPErrorResponse
// @Failure      422  {object}  utils.HTTPErrorResponse
// @Failure      500  {object}  utils.HTTPErrorResponse
// @Router       /v1/instance/{id}/status [get]
// @Router       /v1/instance/connectionState/{id} [get]
func (s *Instance) Status(ctx echo.Context) error {
	c := ctx.Request().Context()
	var request dto.ConnectInstanceRequest
	if err := ctx.Bind(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusUnprocessableEntity, err, "failed to bind request body")
	}

	result, err := s.repo.List(c, request.ID)
	if err != nil {
		zap.L().Error("failed to list instances", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to list instances")
	}

	if len(result) == 0 {
		return utils.HTTPFail(ctx, http.StatusNotFound, err, "instance not found")
	}

	status, err := s.whatsmiau.Status(request.ID)
	if err != nil {
		zap.L().Error("failed to get status instance", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to get status instance")
	}

	return ctx.JSON(http.StatusOK, dto.StatusInstanceResponse{
		ID:     request.ID,
		Status: string(status),
		Instance: &dto.StatusInstanceResponseEvolutionCompatibility{
			InstanceName: request.ID,
			State:        string(status),
		},
	})
}

// Logout godoc
// @Summary      Logout an instance
// @Description  Disconnects the WhatsApp session for the given instance without deleting it
// @Tags         Instance
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id  path      string  true  "Instance ID"
// @Success      200  {object}  dto.DeleteInstanceResponse
// @Failure      404  {object}  utils.HTTPErrorResponse
// @Failure      422  {object}  utils.HTTPErrorResponse
// @Failure      500  {object}  utils.HTTPErrorResponse
// @Router       /v1/instance/{id}/logout [post]
// @Router       /v1/instance/logout/{id} [delete]
func (s *Instance) Logout(ctx echo.Context) error {
	c := ctx.Request().Context()
	var request dto.DeleteInstanceRequest
	if err := ctx.Bind(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusUnprocessableEntity, err, "failed to bind request body")
	}

	result, err := s.repo.List(c, request.ID)
	if err != nil {
		zap.L().Error("failed to list instances", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to list instances")
	}

	if len(result) == 0 {
		return utils.HTTPFail(ctx, http.StatusNotFound, err, "instance not found")
	}

	if err := s.whatsmiau.Disconnect(request.ID); err != nil {
		zap.L().Error("failed to logout instance", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to logout instance")
	}

	return ctx.JSON(http.StatusOK, dto.DeleteInstanceResponse{
		Message: "instance logout successfully",
	})
}

// Delete godoc
// @Summary      Delete an instance
// @Description  Disconnects and permanently removes the specified instance
// @Tags         Instance
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id  path      string  true  "Instance ID"
// @Success      200  {object}  dto.DeleteInstanceResponse
// @Failure      422  {object}  utils.HTTPErrorResponse
// @Failure      500  {object}  utils.HTTPErrorResponse
// @Router       /v1/instance/{id} [delete]
// @Router       /v1/instance/delete/{id} [delete]
func (s *Instance) Delete(ctx echo.Context) error {
	c := ctx.Request().Context()
	var request dto.DeleteInstanceRequest
	if err := ctx.Bind(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusUnprocessableEntity, err, "failed to bind request body")
	}

	result, err := s.repo.List(c, request.ID)
	if err != nil {
		zap.L().Error("failed to list instances", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to list instances")
	}

	if len(result) == 0 {
		return ctx.JSON(http.StatusOK, dto.DeleteInstanceResponse{
			Message: "instance doesn't exists",
		})
	}

	// Full teardown (device store, Redis, route, session.lost webhook), same as before.
	if err := s.whatsmiau.Disconnect(request.ID); err != nil {
		zap.L().Error("failed to delete instance", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to delete instance")
	}

	return ctx.JSON(http.StatusOK, dto.DeleteInstanceResponse{
		Message: "instance deleted",
	})
}

// Restart godoc
// @Summary      Restart an instance connection
// @Description  Disconnects and reconnects the WhatsApp websocket without logging out
// @Tags         Instance
// @Produce      json
// @Security     ApiKeyAuth
// @Param        id  path  string  true  "Instance ID"
// @Success      200  {object}  dto.RestartInstanceResponse
// @Failure      400  {object}  utils.HTTPErrorResponse
// @Failure      404  {object}  utils.HTTPErrorResponse
// @Failure      422  {object}  utils.HTTPErrorResponse
// @Failure      500  {object}  utils.HTTPErrorResponse
// @Router       /v1/instance/{id}/restart [post]
// @Router       /v1/instance/restart/{id} [post]
func (s *Instance) Restart(ctx echo.Context) error {
	c := ctx.Request().Context()
	var request dto.RestartInstanceRequest
	if err := ctx.Bind(&request); err != nil {
		return utils.HTTPFail(ctx, http.StatusUnprocessableEntity, err, "failed to bind request body")
	}

	result, err := s.repo.List(c, request.ID)
	if err != nil {
		zap.L().Error("failed to list instances", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to list instances")
	}
	if len(result) == 0 {
		return utils.HTTPFail(ctx, http.StatusNotFound, nil, "instance not found")
	}

	if err := s.whatsmiau.Restart(c, request.ID); err != nil {
		zap.L().Error("failed to restart instance", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to restart instance")
	}

	return ctx.JSON(http.StatusOK, dto.RestartInstanceResponse{
		ID:     request.ID,
		Status: string(whatsmiau.Connecting),
		Instance: &dto.RestartInstanceEvoCompatibility{
			InstanceName: request.ID,
			Status:       string(whatsmiau.Connecting),
		},
	})
}

// DeleteAll deletes all instances when no id is provided (called from DELETE /instance with no path id).
func (s *Instance) DeleteAll(ctx echo.Context) error {
	c := ctx.Request().Context()

	all, err := s.repo.List(c, "")
	if err != nil {
		zap.L().Error("failed to list instances", zap.Error(err))
		return utils.HTTPFail(ctx, http.StatusInternalServerError, err, "failed to list instances")
	}

	for _, inst := range all {
		if err := s.whatsmiau.Disconnect(inst.ID); err != nil {
			zap.L().Error("failed to delete instance", zap.String("id", inst.ID), zap.Error(err))
		}
	}

	return ctx.JSON(http.StatusOK, dto.DeleteInstanceResponse{
		Message: "instances deleted",
	})
}
