package handler

import (
	"net/http"

	"github.com/wizier/airvault/internal/service"

	"github.com/labstack/echo/v5"
)

type pairingStateResponse struct {
	MuxerReady bool                `json:"muxerReady"`
	USBDevices []service.USBDevice `json:"usbDevices"`
}

type startPairingRequest struct {
	UDID string `json:"udid"`
}

func (h *Handler) unpairDevice(c *echo.Context) error {
	// Like "all" in backups.go: exactly "true" or the backups stay.
	deleteBackups := c.QueryParam("deleteBackups") == "true"
	if err := h.svc.Unpair(c.Request().Context(), c.Param("udid"), deleteBackups); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) getPairingState(c *echo.Context) error {
	usb, err := h.svc.ListPairableUSB(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, pairingStateResponse{
		MuxerReady: h.svc.MuxerReady(c.Request().Context()),
		USBDevices: usb,
	})
}

func (h *Handler) startPairing(c *echo.Context) error {
	var request startPairingRequest
	if err := echo.BindBody(c, &request); err != nil {
		return err
	}
	runID, err := h.svc.StartTrustFlow(c.Request().Context(), request.UDID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, acceptedRunResponse{RunID: runID})
}
