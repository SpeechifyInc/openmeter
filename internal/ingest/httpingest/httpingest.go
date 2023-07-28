package httpingest

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/cloudevents/sdk-go/v2/event"
	"golang.org/x/exp/slog"

	"github.com/openmeterio/openmeter/api"
	"github.com/openmeterio/openmeter/internal/namespace"
	"github.com/openmeterio/openmeter/pkg/models"
)

// Handler receives an event in CloudEvents format and forwards it to a {Collector}.
type Handler struct {
	Collector Collector

	Logger *slog.Logger
}

// Collector is a receiver of events that handles sending those events to some downstream broker.
type Collector interface {
	Receive(ev event.Event, namespace string) error
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, params api.IngestEventsParams) {
	logger := h.getLogger()

	var event event.Event

	err := json.NewDecoder(r.Body).Decode(&event)
	if err != nil {
		logger.ErrorCtx(r.Context(), "unable to parse event", "error", err)

		models.NewStatusProblem(r.Context(), err, http.StatusInternalServerError).Respond(w, r)
		return
	}

	logger = logger.With(
		slog.String("event_id", event.ID()),
		slog.String("event_subject", event.Subject()),
		slog.String("event_source", event.Source()),
	)

	if event.Time().IsZero() {
		logger.DebugCtx(r.Context(), "event does not have a timestamp")

		event.SetTime(time.Now().UTC())
	}

	namespace := namespace.DefaultNamespace
	if params.NamespaceInput != nil {
		namespace = *params.NamespaceInput
	}

	err = h.Collector.Receive(event, namespace)
	if err != nil {
		logger.ErrorCtx(r.Context(), "unable to forward event to collector", "error", err)

		models.NewStatusProblem(r.Context(), err, http.StatusInternalServerError).Respond(w, r)
		return
	}

	logger.InfoCtx(r.Context(), "event forwarded to downstream collector")

	w.WriteHeader(http.StatusOK)
}

func (h Handler) getLogger() *slog.Logger {
	logger := h.Logger

	if logger == nil {
		logger = slog.Default()
	}

	return logger
}
