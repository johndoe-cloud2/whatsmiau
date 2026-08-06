package instances

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/verbeux-ai/whatsmiau/interfaces"
	"github.com/verbeux-ai/whatsmiau/models"
	"golang.org/x/net/context"
)

// These verify if RedisInstance follows instances interface pattern
var _ interfaces.InstanceRepository = (*RedisInstance)(nil)

var ErrorNotFound = errors.New("not found")
var ErrorAlreadyExists = errors.New("instance already exists")

type RedisInstance struct {
	db *redis.Client
}

func (s *RedisInstance) key(id string) string {
	return fmt.Sprintf("instance_%s", id)
}

func (s *RedisInstance) keyLastActivity(id string) string {
	return fmt.Sprintf("instance_%s_last_activity", id)
}

func NewRedis(client *redis.Client) *RedisInstance {
	return &RedisInstance{
		db: client,
	}
}

func (s *RedisInstance) Create(ctx context.Context, instance *models.Instance) error {
	if instance.ID == "" {
		return ErrInstanceIDEmpty
	}

	result, err := s.List(ctx, instance.ID)
	if err != nil {
		return err
	}

	if len(result) > 0 {
		return ErrorAlreadyExists
	}

	data, err := json.Marshal(instance)
	if err != nil {
		return err
	}
	return s.db.Set(ctx, s.key(instance.ID), data, redis.KeepTTL).Err()
}

func (s *RedisInstance) Update(ctx context.Context, id string, toUpdate *models.Instance) (*models.Instance, error) {
	if id == "" {
		return nil, ErrInstanceIDEmpty
	}

	result, err := s.List(ctx, id)
	if err != nil {
		return nil, err
	}

	if len(result) <= 0 {
		return nil, ErrorNotFound
	}

	oldInstance := result[0]
	if len(toUpdate.RemoteJID) > 0 {
		oldInstance.RemoteJID = toUpdate.RemoteJID
	}
	if toUpdate.Webhook.Enabled != nil {
		oldInstance.Webhook.Enabled = toUpdate.Webhook.Enabled
	}
	if toUpdate.Webhook.Url != "" {
		oldInstance.Webhook.Url = toUpdate.Webhook.Url
	}
	if toUpdate.Webhook.ByEvents != nil {
		oldInstance.Webhook.ByEvents = toUpdate.Webhook.ByEvents
	}
	if toUpdate.Webhook.Base64 != nil {
		oldInstance.Webhook.Base64 = toUpdate.Webhook.Base64
	}
	if toUpdate.Webhook.Headers != nil {
		if oldInstance.Webhook.Headers == nil {
			oldInstance.Webhook.Headers = map[string]string{}
		}
		for k, v := range toUpdate.Webhook.Headers {
			oldInstance.Webhook.Headers[k] = v
		}
	}
	if toUpdate.Webhook.Events != nil {
		oldInstance.Webhook.Events = toUpdate.Webhook.Events
	}

	if toUpdate.ProxyHost != "" {
		oldInstance.InstanceProxy = toUpdate.InstanceProxy
	}

	if toUpdate.ProxyHost == "" {
		if toUpdate.ProxyPort != "" {
			oldInstance.ProxyPort = toUpdate.ProxyPort
		}
		if toUpdate.ProxyProtocol != "" {
			oldInstance.ProxyProtocol = toUpdate.ProxyProtocol
		}
		if toUpdate.ProxyUsername != "" {
			oldInstance.ProxyUsername = toUpdate.ProxyUsername
		}
		if toUpdate.ProxyPassword != "" {
			oldInstance.ProxyPassword = toUpdate.ProxyPassword
		}
	}

	data, err := json.Marshal(oldInstance)
	if err != nil {
		return nil, err
	}

	return &oldInstance, s.db.Set(ctx, s.key(id), data, redis.KeepTTL).Err()
}

func (s *RedisInstance) List(ctx context.Context, id string) ([]models.Instance, error) {
	// Redis applies SCAN's MATCH after walking the keyspace, so scanning for a single exact key
	// still costs a full pass (keyspace/COUNT round-trips). Fetch it directly instead: this runs
	// on every WhatsApp event via getInstanceCached.
	if len(id) > 0 {
		raw, err := s.db.Get(ctx, s.key(id)).Result()
		if errors.Is(err, redis.Nil) {
			return []models.Instance{}, nil
		}
		if err != nil {
			return nil, err
		}

		var inst models.Instance
		if err := json.Unmarshal([]byte(raw), &inst); err != nil {
			return []models.Instance{}, nil
		}

		return []models.Instance{inst}, nil
	}

	var (
		cursor uint64
		keys   []string
	)

	for {
		batch, newCursor, err := s.db.Scan(ctx, cursor, "instance_*", 100).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		cursor = newCursor
		if cursor == 0 {
			break
		}
	}

	if len(keys) == 0 {
		return []models.Instance{}, nil
	}

	rawVals, err := s.db.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}

	var instances []models.Instance
	for _, raw := range rawVals {
		if raw == nil {
			continue
		}
		strVal, ok := raw.(string)
		if !ok {
			continue
		}
		var inst models.Instance
		if err := json.Unmarshal([]byte(strVal), &inst); err != nil {
			continue
		}
		instances = append(instances, inst)
	}

	return instances, nil
}

func (s *RedisInstance) Delete(ctx context.Context, id string) error {
	if id == "" {
		return ErrInstanceIDEmpty
	}

	result, err := s.List(ctx, id)
	if err != nil {
		return err
	}

	if len(result) <= 0 {
		return ErrorNotFound
	}

	// Remove instance and its last-activity timestamp
	_ = s.db.Del(ctx, s.keyLastActivity(id)).Err()
	return s.db.Del(ctx, s.key(id)).Err()
}

// TouchLastWebhookActivity sets the last time this instance sent an event to the webhook (used for stale cleanup).
func (s *RedisInstance) TouchLastWebhookActivity(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return nil
	}
	return s.db.Set(ctx, s.keyLastActivity(instanceID), time.Now().Unix(), redis.KeepTTL).Err()
}

// ListStaleInstances returns instance IDs that have not sent any webhook event since olderThan ago (or never).
func (s *RedisInstance) ListStaleInstances(ctx context.Context, olderThan time.Duration) ([]string, error) {
	all, err := s.List(ctx, "")
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-olderThan).Unix()
	var stale []string
	for _, inst := range all {
		val, err := s.db.Get(ctx, s.keyLastActivity(inst.ID)).Result()
		if err == redis.Nil {
			// No activity ever recorded -> stale
			stale = append(stale, inst.ID)
			continue
		}
		if err != nil {
			continue
		}
		ts, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			stale = append(stale, inst.ID)
			continue
		}
		if ts < cutoff {
			stale = append(stale, inst.ID)
		}
	}
	return stale, nil
}

const redisKeyBackends = "backends"
const redisKeyRoutePrefix = "route:"

// RegisterBackend adds this backend URL to the Redis set "backends" so the router can route new instances here.
func (s *RedisInstance) RegisterBackend(ctx context.Context, url string) error {
	if url == "" {
		return nil
	}
	return s.db.SAdd(ctx, redisKeyBackends, url).Err()
}

// UnregisterBackend removes this backend URL from the Redis set "backends" (call on shutdown so the router stops proxying here).
func (s *RedisInstance) UnregisterBackend(ctx context.Context, url string) error {
	if url == "" {
		return nil
	}
	return s.db.SRem(ctx, redisKeyBackends, url).Err()
}

// GetAllBackends returns all backend URLs registered in Redis (for cleanup/health checks).
func (s *RedisInstance) GetAllBackends(ctx context.Context) ([]string, error) {
	return s.db.SMembers(ctx, redisKeyBackends).Result()
}

// SetRoute sets route:<instanceID> = backendURL so the router can proxy requests for this instance to this backend.
func (s *RedisInstance) SetRoute(ctx context.Context, instanceID, backendURL string) error {
	if instanceID == "" || backendURL == "" {
		return nil
	}
	return s.db.Set(ctx, redisKeyRoutePrefix+instanceID, backendURL, redis.KeepTTL).Err()
}

// DeleteRoute removes route:<instanceID> (e.g. when session is lost).
func (s *RedisInstance) DeleteRoute(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return nil
	}
	return s.db.Del(ctx, redisKeyRoutePrefix+instanceID).Err()
}

// DeleteRoutesForBackend removes all route:<id> keys whose value equals backendURL.
// Call this on SIGTERM so the router stops proxying instance requests to this (shutting down) backend.
// Returns the number of routes deleted.
func (s *RedisInstance) DeleteRoutesForBackend(ctx context.Context, backendURL string) (int, error) {
	if backendURL == "" {
		return 0, nil
	}
	var cursor uint64
	deleted := 0
	for {
		keys, next, err := s.db.Scan(ctx, cursor, redisKeyRoutePrefix+"*", 100).Result()
		if err != nil {
			return deleted, err
		}
		for _, k := range keys {
			val, err := s.db.Get(ctx, k).Result()
			if err == redis.Nil {
				continue
			}
			if err != nil {
				continue
			}
			if val == backendURL {
				if err := s.db.Del(ctx, k).Err(); err != nil {
					continue
				}
				deleted++
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return deleted, nil
}

// TTL for emitted message keys: avoid re-emitting duplicates for 7 days; keys expire automatically.
const emittedMessageTTL = 7 * 24 * time.Hour

func (s *RedisInstance) keyEmittedMessage(instanceID, messageKey string) string {
	return fmt.Sprintf("emitted:%s:%s", instanceID, messageKey)
}

// TryClaimMessage atomically sets the emitted key only if it does not exist (Redis SETNX).
// Returns true if the caller won the claim (first to process this message), false if already claimed.
func (s *RedisInstance) TryClaimMessage(ctx context.Context, instanceID, messageKey string) (bool, error) {
	if instanceID == "" || messageKey == "" {
		return true, nil
	}
	ok, err := s.db.SetNX(ctx, s.keyEmittedMessage(instanceID, messageKey), "1", emittedMessageTTL).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

// DeleteEmittedMessagesForInstance removes all emitted-message keys for this instance (e.g. on teardown).
func (s *RedisInstance) DeleteEmittedMessagesForInstance(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return nil
	}
	pattern := fmt.Sprintf("emitted:%s:*", instanceID)
	var cursor uint64
	for {
		keys, next, err := s.db.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := s.db.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}
