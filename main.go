package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
)

var (
	mongoClient *mongo.Client
	rdb         *redis.Client
	limiter     *redis_rate.Limiter
)

func initRedis() {
	redisURI := fmt.Sprintf("%s:6379", os.Getenv("REDIS_HOST"))
	rdb = redis.NewClient(&redis.Options{
		Addr:     redisURI,
		Password: "",
		DB:       0,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		panic(fmt.Sprintf("Не удалось подключиться к Redis: %v", err))
	}
	limiter = redis_rate.NewLimiter(rdb)
}

func initMongoDB() {
	var err error
	username := os.Getenv("MONGO_USERNAME")
	password := os.Getenv("MONGO_PASSWORD")
	host := os.Getenv("MONGO_HOST")
	mongoURI := fmt.Sprintf("mongodb://%s:%s@%s:27017", username, password, host)
	fmt.Println("MongoDB URI:", mongoURI)

	clientOptions := options.Client().ApplyURI(mongoURI)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mongoClient, err = mongo.Connect(ctx, clientOptions)
	if err != nil {
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}

	// It's a good practice to ping the MongoDB server to ensure connection is successful
	ctxPing, cancelPing := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelPing()
	if err := mongoClient.Ping(ctxPing, nil); err != nil {
		log.Fatalf("Failed to ping MongoDB: %v", err)
	}

	fmt.Println("Connected to MongoDB!")
}

type BinData struct {
	Country       string `bson:"country"`
	CountryCode   string `bson:"country-code"`
	CardBrand     string `bson:"card-brand"`
	IsCommercial  bool   `bson:"is-commercial"`
	BinNumber     string `bson:"bin-number"`
	Issuer        string `bson:"issuer"`
	IssuerWebsite string `bson:"issuer-website"`
	Valid         bool   `bson:"valid"`
	CardType      string `bson:"card-type"`
	IsPrepaid     bool   `bson:"is-prepaid"`
	CardCategory  string `bson:"card-category"`
	IssuerPhone   string `bson:"issuer-phone"`
	CurrencyCode  string `bson:"currency-code"`
	CountryCode3  string `bson:"country-code3"`
}

func isValidBIN(number string) bool {
	if len(number) < 6 {
		return false
	}

	for _, r := range number {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func getFromDB(bin string) (*BinData, error) {
	collection := mongoClient.Database("bin-lookup-gateway").Collection("bins")

	if len(bin) > 6 {
		bin = bin[:6]
	}
	regexPattern := "^" + bin

	filter := bson.D{{"bin-number", bson.D{{"$regex", regexPattern}}}}

	opts := options.FindOne().SetSort(bson.D{{"bin-number", -1}})

	var result BinData
	err := collection.FindOne(context.Background(), filter, opts).Decode(&result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func saveToDB(binData *BinData) error {
	collection := mongoClient.Database("bin-lookup-gateway").Collection("bins")

	_, err := collection.InsertOne(context.Background(), binData)
	if err != nil {
		return err
	}
	return nil
}

func makeRequest(client *http.Client, reqURL string, bin string) *BinData {
	params := url.Values{}
	params.Add("bin-number", bin)

	req, err := http.NewRequest("GET", reqURL+"?"+params.Encode(), nil)
	if err != nil {
		log.Printf("failed to create request: %v", err)
		return nil
	}
	req.Header.Add("user-id", os.Getenv("NEUTRINOAPI_USER_ID"))
	req.Header.Add("api-key", os.Getenv("NEUTRINOAPI_API_KEY"))
	req.Header.Add("Accept", "application/json")

	fmt.Println("Requesting data for BIN/IIN number:", bin)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("request failed: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("received non-200 response: %d", resp.StatusCode)
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("failed to read response body: %v", err)
		return nil
	}
	var binData *BinData

	err = bson.UnmarshalExtJSON(body, true, &binData)
	if err != nil {
		log.Printf("failed to unmarshal response body: %v", err)
		return nil
	}
	return binData
}

func requestHandler(client *http.Client, reqURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bin := strings.TrimSpace(r.URL.Query().Get("bin"))
		if !isValidBIN(bin) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Invalid BIN number"))
			return
		}
		binData, _ := getFromDB(bin)
		if binData != nil {
			jsonData, err := json.Marshal(binData)
			if err != nil {
				http.Error(w, "Failed to encode BIN data as JSON", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(jsonData)
			return
		}
		res, err := limiter.Allow(context.Background(), "bin-lookup-gateway", redis_rate.PerSecond(100))
		if err != nil {
			log.Printf("Rate limiter error: %v", err)
			http.Error(w, "Server error", http.StatusInternalServerError)
			return
		}
		if res.Allowed == 0 {
			// Not allowed to proceed
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		binData = makeRequest(client, reqURL, bin)
		if binData == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("No data found for this BIN/IIN number"))
			return
		}
		if binData.BinNumber == "" {
			binData.BinNumber = bin[:6]
		}
		err = saveToDB(binData)
		if err != nil {
			log.Printf("failed to save data to DB: %v", err)
		}
		jsonData, err := json.Marshal(binData)
		if err != nil {
			http.Error(w, "Failed to encode BIN data as JSON", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(jsonData)
	}
}

func main() {
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              os.Getenv("BIN_LOOKUP_GATEWAY_SENTRY_DSN"),
		EnableTracing:    true,
		TracesSampleRate: 1.0,
	}); err != nil {
		fmt.Printf("Sentry initialization failed: %v", err)
	}
	initMongoDB()
	initRedis()
	defer func() {
		if err := mongoClient.Disconnect(context.Background()); err != nil {
			log.Fatalf("Error on disconnection with MongoDB: %v", err)
		}
	}()
	sentryHandler := sentryhttp.New(sentryhttp.Options{})

	client := &http.Client{}
	reqURL := "https://neutrinoapi.net/bin-lookup"
	http.HandleFunc("/", sentryHandler.HandleFunc(requestHandler(client, reqURL)))

	log.Println("Server starting on port :8080...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}
