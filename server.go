/*
Package sse provides utilities for creating and consuming fully spec-compliant HTML5 server-sent events streams.

The central piece of a server's implementation is the Provider interface. A Provider describes a publish-subscribe
system that can be used to implement messaging for the SSE protocol. This package already has an
implementation, called Joe, that is the default provider for any server. Abstracting the messaging
system implementation away allows servers to use any arbitrary provider under the same interface.
The default provider will work for simple use-cases, but where scalability is required, one will
look at a more suitable solution. Adapters that satisfy the Provider interface can easily be created,
and then plugged into the server instance.
Events themselves are represented using the Message type.

On the client-side, we use the Client struct to create connections to event streams. Using an `http.Request`
we instantiate a Connection. Then we subscribe to incoming events using callback functions, and then
we establish the connection by calling the Connection's Connect method.
*/
package sse

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"golang.org/x/exp/slog"
)

// The Subscription struct is used to subscribe to a given provider.
type Subscription struct {
	// The client to which messages are sent. The implementation of the interface does not have to be
	// thread-safe – providers will not call methods on it concurrently.
	Client MessageWriter
	// An optional last event ID indicating the event to resume the stream from.
	// The events will replay starting from the first valid event sent after the one with the given ID.
	// If the ID is invalid replaying events will be omitted and new events will be sent as normal.
	LastEventID EventID
	// The topics to receive message from. If no topic is specified, a default topic is implied.
	// Topics are orthogonal to event types. They are used to filter what the server sends to each client.
	//
	// If using a Provider directly, without a Server instance, you must specify at least one topic.
	// The Server automatically adds the default topic if no topic is specified.
	Topics []string
}

// A Provider is a publish-subscribe system that can be used to implement a HTML5 server-sent events
// protocol. A standard interface is required so HTTP request handlers are agnostic to the provider's implementation.
//
// Providers are required to be thread-safe.
//
// After Stop is called, trying to call any method of the provider must return ErrProviderClosed. The providers
// may return other implementation-specific errors too, but the close error is guaranteed to be the same across
// providers.
type Provider interface {
	// Subscribe to the provider. The context is used to remove the subscriber automatically
	// when it is done. Errors returned by the subscription's callback function must be returned
	// by Subscribe.
	//
	// Providers can assume that the topics list for a subscription has at least one topic.
	Subscribe(ctx context.Context, subscription Subscription) error
	// Publish a message to all the subscribers that are subscribed to the given topics.
	// The topics slice must be non-empty, or ErrNoTopic will be raised.
	Publish(message *Message, topics []string) error
	// Shutdown stops the provider. Calling Shutdown will clean up all the provider's resources
	// and make Subscribe and Publish fail with an error. All the listener channels will be
	// closed and any ongoing publishes will be aborted.
	//
	// If the given context times out before the provider is shut down – shutting it down takes
	// longer, the context error is returned.
	//
	// Calling Shutdown multiple times after it successfully returned the first time
	// does nothing but return ErrProviderClosed.
	Shutdown(ctx context.Context) error
}

// ErrProviderClosed is a sentinel error returned by providers when any operation is attempted after the provider is closed.
var ErrProviderClosed = errors.New("go-sse.server: provider is closed")

// ErrNoTopic is a sentinel error returned by providers when a Message is published without any topics.
// It is not an issue to call Server.Publish without topics, because the Server will add the DefaultTopic;
// it is an error to call Provider.Publish without any topics, though.
var ErrNoTopic = errors.New("go-sse.server: no topics specified")

// DefaultTopic is the identifier for the topic that is implied when no topics are specified for a Subscription
// or a Message. Providers are required to implement this behavior to ensure handlers don't break if providers
// are changed.
const DefaultTopic = ""

// A Server is mostly a convenience wrapper around a provider.
// It implements the http.Handler interface and has some methods
// for calling the underlying provider's methods.
//
// When creating a server, if no provider is specified using the WithProvider
// option, the Joe provider found in this package with no replay provider is used.
type Server struct {
	// The provider used to publish and subscribe clients to events.
	// Defaults to Joe.
	Provider Provider
	// A callback that's called when a SSE session is started.
	// You can use this to authorize the session, set the topics
	// the client should be subscribed to and so on. Using the
	// Res field of the Session you can write an error response
	// to the client.
	//
	// The boolean returned indicates whether the returned subscription
	// is valid or not. If it is valid, the Provider will receive it
	// and events will be sent to this client, otherwise the request
	// will be ended.
	//
	// If this is not set, the client will be subscribed to the provider
	// using the DefaultTopic.
	OnSession func(*Session) (Subscription, bool)
	// Logger can be used to get a custom logger from the request context,
	// which could have been set beforehand through a middleware, for example.
	// By default, nothing is logged by the server. If this function is present
	// and returns a non-nil logger, then info will be logged. Add to the logger
	// the data you want to be logged together with what the library adds,
	// for example identification info like request IP, origin etc.
	Logger func(*http.Request) *slog.Logger

	provider Provider
	initDone sync.Once
}

// ServeHTTP implements a default HTTP handler for a server.
//
// This handler upgrades the request, subscribes it to the server's provider and
// starts sending incoming events to the client, while logging any errors.
// It also sends the Last-Event-ID header's value, if present.
//
// If the request isn't upgradeable, it writes a message to the client along with
// an 500 Internal Server ConnectionError response code. If on subscribe the provider returns
// an error, it writes the error message to the client and a 500 Internal Server ConnectionError
// response code.
//
// To customize behavior, use the OnSession callback or create your custom handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.init()
	// Make sure to keep the ServeHTTP implementation line number in sync with the number in the README!

	l := s.logger(r)
	if l != nil {
		l.InfoContext(r.Context(), "sse: starting new session")
	}

	sess, err := Upgrade(w, r)
	if err != nil {
		if l != nil {
			l.ErrorContext(r.Context(), "sse: unsupported")
		}

		http.Error(w, "Server-sent events unsupported", http.StatusInternalServerError)
		return
	}

	sub, ok := s.getSubscription(sess)
	if !ok {
		if l != nil {
			l.WarnContext(r.Context(), "sse: invalid subscription")
		}

		return
	}

	if l != nil {
		l.InfoContext(r.Context(), "sse: subscribing session", "topics", getTopicsLog(sub.Topics), "lastEventID", sub.LastEventID)
	}

	if err = s.provider.Subscribe(r.Context(), sub); err != nil {
		if l != nil {
			l.ErrorContext(r.Context(), "sse: subscribe error", "err", err)
		}

		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if l != nil {
		l.InfoContext(r.Context(), "sse: session ended")
	}
}

// Publish sends the event to all subscribes that are subscribed to the topic the event is published to.
// The topics are optional - if none are specified, the event is published to the DefaultTopic.
func (s *Server) Publish(e *Message, topics ...string) error {
	s.init()
	return s.provider.Publish(e, getTopics(topics))
}

// Shutdown closes all the connections and stops the server. Publish operations will fail
// with the error sent by the underlying provider. NewServer requests will be ignored.
//
// Call this method when shutting down the HTTP server using http.Server's RegisterOnShutdown
// method. Not doing this will result in the server never shutting down or connections being
// abruptly stopped.
//
// See the Provider.Shutdown documentation for information on context usage and errors.
func (s *Server) Shutdown(ctx context.Context) error {
	s.init()
	return s.provider.Shutdown(ctx)
}

func (s *Server) init() {
	s.initDone.Do(func() {
		s.provider = s.Provider
		if s.provider == nil {
			s.provider = &Joe{}
		}
	})
}

func (s *Server) getSubscription(sess *Session) (Subscription, bool) {
	if s.OnSession != nil {
		return s.OnSession(sess)
	}

	return Subscription{
		Client:      sess,
		LastEventID: sess.LastEventID,
		Topics:      defaultTopicSlice,
	}, true
}

func (s *Server) logger(r *http.Request) *slog.Logger {
	if s.Logger != nil {
		return s.Logger(r)
	}

	return nil
}

var defaultTopicSlice = []string{DefaultTopic}

func getTopics(initial []string) []string {
	if len(initial) == 0 {
		return defaultTopicSlice
	}

	return initial
}

func getTopicsLog(topics []string) string {
	seen := map[string]struct{}{}
	ret := ""

	for i, t := range topics {
		if _, ok := seen[t]; ok {
			continue
		}

		seen[t] = struct{}{}

		if i > 0 {
			ret += ","
		}

		if t == DefaultTopic {
			ret += "<sse:default>"
		} else {
			ret += t
		}
	}

	return ret
}
