package sse

import (
	"context"
	"sync"
	"time"
)

// A ReplayProvider is a type that can replay older published events to new subscribers.
// Replay providers use event IDs, the topics the events were published to and optionally
// the events' expiration times or any other criteria to determine which are valid for replay.
//
// While providers can require events to have IDs beforehand, they can also set the IDs themselves,
// automatically - it's up to the implementation. Providers should ignore events without IDs,
// if they require IDs to be set.
//
// Replay providers are not required to be thread-safe - server providers are required to ensure only
// one operation is executed on the replay provider at any given time. Server providers may not execute
// replay operation concurrently with other operations, so make sure any action on the replay provider
// blocks for as little as possible. If a replay provider is thread-safe, some operations may be
// run in a separate goroutine - see the interface's method documentation.
//
// Executing actions that require waiting for a long time on I/O, such as HTTP requests or database
// calls must be handled with great care, so the server provider is not blocked. Reducing them to
// the minimum by using techniques such as caching or by executing them in separate goroutines is
// recommended, as long as the implementation fulfills the requirements.
//
// If not specified otherwise, the errors returned are implementation-specific.
type ReplayProvider interface {
	// Put adds a new event to the replay buffer. The Message that is returned may not have the
	// same address, if the replay provider automatically sets IDs.
	//
	// Put panics if the message couldn't be queued – if no topics are provided, or
	// a message without an ID is put into a ReplayProvider which does not
	// automatically set IDs.
	//
	// The Put operation may be executed by the replay provider in another goroutine only if
	// it can ensure that any Replay operation called after the Put goroutine is started
	// can replay the new received message. This also requires the replay provider implementation
	// to be thread-safe.
	//
	// Replay providers are not required to guarantee that after Put returns the new events
	// can be replayed. If an error occurs internally when putting the new message
	// and retrying the operation would block for too long, it can be aborted.
	// The errors aren't returned as the server providers won't be able to handle them in a useful manner.
	Put(message *Message, topics []string) *Message
	// Replay sends to a new subscriber all the valid events received by the provider
	// since the event with the listener's ID. If the ID the listener provides
	// is invalid, the provider should not replay any events.
	//
	// Replay operations must be executed in the same goroutine as the one it is called in.
	// Other goroutines may be launched from inside the Replay method, but the events must
	// be sent to the listener in the same goroutine that Replay is called in.
	//
	// If an error is returned, then at least some messages weren't successfully replayed.
	// The error is nil if there were no messages to replay for the particular subscription
	// or if all messages were replayed successfully.
	Replay(subscription Subscription) error
}

// ReplayProviderWithGC is a ReplayProvider that must have invalid messages cleaned up from time to time.
// This may be the case for a provider that replays messages that are not expired: at a certain interval,
// expired messages must be removed from the provider to free up resources.
//
// Providers must check if replay providers implement this interface, so they can call GC accordingly.
type ReplayProviderWithGC interface {
	ReplayProvider
	// GC triggers a cleanup. After GC returns, all the messages that are invalid according
	// to the provider's criteria should be impossible to replay again.
	//
	// If GC returns an error, the provider is not required to try to trigger another
	// GC ever again. Make sure that before you return a non-nil value you handle
	// temporary errors accordingly, with blocking as shortly as possible.
	//
	// If the replay provider implementation is thread-safe the GC operation can be executed in another goroutine.
	GC() error
}

type (
	subscriber   chan<- error
	subscribers  map[subscriber]MessageWriter
	subscription struct {
		done subscriber
		Subscription
	}

	messageWithTopics struct {
		message *Message
		topics  []string
	}
)

// Joe is a basic server provider that synchronously executes operations by queueing them in channels.
// Events are also sent synchronously to subscribers, so if a subscriber's callback blocks, the others
// have to wait.
//
// Joe optionally supports event replaying with the help of a replay provider.
//
// If due to some unexpected scenario (the replay provider has a bug, for example) a panic occurs,
// Joe will remove all subscribers, so requests don't hang.
//
// He serves simple use-cases well, as he's light on resources, and does not require any external
// services. Also, he is the default provider for Servers.
type Joe struct {
	message        chan messageWithTopics
	subscription   chan subscription
	unsubscription chan subscriber
	done           chan struct{}
	closed         chan struct{}
	topics         map[string]subscribers

	// An optional replay provider that Joe uses to resend older messages to new subscribers.
	ReplayProvider ReplayProvider
	// An optional interval at which Joe triggers a cleanup of expired messages, if the replay provider supports it.
	// See the desired provider's documentation to determine if periodic cleanup is necessary.
	ReplayGCInterval time.Duration

	initDone sync.Once
}

// Subscribe tells Joe to send new messages to this subscriber. The subscription
// is automatically removed when the context is done, a callback error occurs
// or Joe is stopped.
func (j *Joe) Subscribe(ctx context.Context, sub Subscription) error {
	j.init()

	done := make(chan error, 1)

	select {
	case <-j.done:
		return ErrProviderClosed
	case j.subscription <- subscription{done: done, Subscription: sub}:
	}

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	select {
	case err := <-done:
		return err
	case j.unsubscription <- done:
		return nil
	}
}

// Publish tells Joe to send the given message to the subscribers.
// When a message is published to multiple topics, Joe makes sure to
// not send the Message multiple times to clients that are subscribed
// to more than one topic that receive the given Message. Every client
// receives each unique message once, regardless of how many topics it
// is subscribed to or to how many topics the message is published.
func (j *Joe) Publish(msg *Message, topics []string) error {
	if len(topics) == 0 {
		return ErrNoTopic
	}

	j.init()

	// Waiting on done ensures Publish doesn't block the caller goroutine
	// when Joe is stopped and implements the required Provider behavior.
	select {
	case j.message <- messageWithTopics{message: msg, topics: topics}:
		return nil
	case <-j.done:
		return ErrProviderClosed
	}
}

// Stop signals Joe to close all subscribers and stop receiving messages.
// It returns when all the subscribers are closed.
//
// Further calls to Stop will return ErrProviderClosed.
func (j *Joe) Shutdown(ctx context.Context) (err error) {
	j.init()

	defer func() {
		if r := recover(); r != nil {
			err = ErrProviderClosed
		}
	}()

	close(j.done)

	select {
	case <-j.closed:
	case <-ctx.Done():
		err = ctx.Err()
	}

	return
}

func (j *Joe) topic(identifier string) subscribers {
	if _, ok := j.topics[identifier]; !ok {
		j.topics[identifier] = subscribers{}
	}
	return j.topics[identifier]
}

func (j *Joe) removeSubscriber(sub subscriber) {
	for _, subs := range j.topics {
		delete(subs, sub)
	}

	close(sub)
}

func (j *Joe) start(replay ReplayProvider, gcFn func() error, gcSignal <-chan time.Time, stopGCSignal func()) {
	defer close(j.closed)
	// defer closing all subscribers instead of closing them when done is closed
	// so in case of a panic subscribers won't block the request goroutines forever.
	defer j.closeSubscribers()
	defer stopGCSignal()

	for {
		select {
		case msg := <-j.message:
			toDispatch := replay.Put(msg.message, msg.topics)
			seen := map[subscriber]struct{}{}

			for _, topic := range msg.topics {
				for done, c := range j.topics[topic] {
					if _, ok := seen[done]; ok {
						continue
					}

					err := c.Send(toDispatch)
					if err == nil {
						err = c.Flush()
					}

					if err != nil {
						done <- err
						j.removeSubscriber(done)
					} else {
						seen[done] = struct{}{}
					}
				}
			}
		case sub := <-j.subscription:
			if err := replay.Replay(sub.Subscription); err != nil {
				sub.done <- err
				close(sub.done)
				continue
			}

			for _, topic := range sub.Topics {
				j.topic(topic)[sub.done] = sub.Client
			}
		case sub := <-j.unsubscription:
			j.removeSubscriber(sub)
		case <-gcSignal:
			if err := gcFn(); err != nil {
				stopGCSignal()
			}
		case <-j.done:
			return
		}
	}
}

func (j *Joe) closeSubscribers() {
	seen := map[subscriber]struct{}{}

	for _, subs := range j.topics {
		for sub := range subs {
			if _, ok := seen[sub]; ok {
				continue
			}

			seen[sub] = struct{}{}
			close(sub)
		}
	}
}

func (j *Joe) init() {
	j.initDone.Do(func() {
		j.message = make(chan messageWithTopics)
		j.subscription = make(chan subscription)
		j.unsubscription = make(chan subscriber)
		j.done = make(chan struct{})
		j.closed = make(chan struct{})
		j.topics = map[string]subscribers{}

		replay := j.ReplayProvider
		if replay == nil {
			replay = noopReplayProvider{}
		}

		var gcFn func() error
		replayGCInterval := j.ReplayGCInterval

		provider, hasGC := replay.(ReplayProviderWithGC)
		if hasGC {
			gcFn = provider.GC
		} else {
			replayGCInterval = 0
		}

		gc, stopGCTicker := ticker(replayGCInterval)

		go j.start(replay, gcFn, gc, stopGCTicker)
	})
}

// ticker creates a time.Ticker, if duration is positive, and returns its channel and stop function.
// If the duration is negative, it returns a nil channel and a noop function.
func ticker(duration time.Duration) (ticks <-chan time.Time, stop func()) {
	if duration <= 0 {
		return nil, func() {}
	}
	t := time.NewTicker(duration)
	return t.C, t.Stop
}
