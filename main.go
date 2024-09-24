package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aws_request_duration_seconds",
			Help:    "Time spent in requests for aws.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
		},
		[]string{"service", "method", "status"},
	)
)

func init() {
	prometheus.MustRegister(requestDuration)
}

func main() {
	// Create a channel to receive OS signals
	sigs := make(chan os.Signal, 1)
	// Register the channel to receive SIGINT, SIGTERM signals
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	if err := Run(ContextWithSignal(context.Background(), sigs)); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

func ContextWithSignal(ctx context.Context, sigs chan os.Signal) context.Context {
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		select {
		case <-sigs:
			fmt.Println("Received signal, exiting...")
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx
}

func Run(ctx context.Context, opts ...Option) error {
	var (
		httpServerGracefulShutdownTimeout = 5 * time.Second

		httpServer = &http.Server{Addr: ":8080"}
		listenErr  = make(chan error, 1)
	)

	http.Handle("/metrics", promhttp.Handler())
	go func() {
		listenErr <- httpServer.ListenAndServe()
	}()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("unable to load SDK config, %v", err)
	}

	chkr := newChecker(cfg, opts...)
	checkCtx, checkCancel := context.WithCancel(ctx)

	for {
		select {
		case <-ctx.Done():
			fmt.Println("Context is canceled, exiting...")

			// Stop the checker
			checkCancel()

			// Graceful shutdown
			ctx, cancel := context.WithTimeout(context.Background(), httpServerGracefulShutdownTimeout)
			defer cancel()

			if err := httpServer.Shutdown(ctx); err != nil {
				return fmt.Errorf("failed to shutdown http server, %v", err)
			}

			log.Printf("[DEBUG] HTTP server shut down with this return value: %v", <-listenErr)

			log.Printf("HTTP server shut down successfully")

			return nil
		case <-time.After(1 * time.Second):
			chkr.doCheck(checkCtx)
		}
	}
}

type checker struct {
	s3Client     *s3.Client
	dynamoClient *dynamodb.Client
	sqsClient    *sqs.Client

	s3Bucket      string
	s3Key         string
	dynamodbTable string
	sqsQueueURL   string

	s3Opts       []func(*s3.Options)
	sqsOpts      []func(*sqs.Options)
	dynamodbOpts []func(*dynamodb.Options)
}

type Option func(*checker)

func newChecker(cfg aws.Config, opts ...Option) *checker {
	c := &checker{}
	for _, opt := range opts {
		opt(c)
	}

	c.s3Client = s3.NewFromConfig(cfg, c.s3Opts...)
	c.dynamoClient = dynamodb.NewFromConfig(cfg, c.dynamodbOpts...)
	c.sqsClient = sqs.NewFromConfig(cfg, c.sqsOpts...)

	c.s3Bucket = os.Getenv("S3_BUCKET")
	c.s3Key = os.Getenv("S3_KEY")
	c.dynamodbTable = os.Getenv("DYNAMODB_TABLE")
	c.sqsQueueURL = os.Getenv("SQS_QUEUE_URL")

	return c
}

func (c *checker) doCheck(ctx context.Context) {
	// S3 GetObject
	getStart := time.Now()
	_, err := c.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &c.s3Bucket,
		Key:    &c.s3Key,
	})
	getDuration := time.Since(getStart).Seconds()
	if ctx.Err() == context.Canceled {
		log.Printf("context is canceled")
		return
	} else if err != nil {
		log.Printf("failed to get object, %v", err)
		requestDuration.WithLabelValues("S3", "GetObject", "Failure").Observe(getDuration)
	} else {
		requestDuration.WithLabelValues("S3", "GetObject", "Success").Observe(getDuration)
	}

	// DynamoDB Scan
	dynamoStart := time.Now()
	_, err = c.dynamoClient.Scan(ctx, &dynamodb.ScanInput{
		TableName: &c.dynamodbTable,
	})
	dynamoDuration := time.Since(dynamoStart).Seconds()
	if ctx.Err() == context.Canceled {
		log.Printf("context is canceled")
		return
	} else if err != nil {
		log.Printf("failed to get item, %v", err)
		requestDuration.WithLabelValues("DynamoDB", "Scan", "Failure").Observe(dynamoDuration)
	} else {
		requestDuration.WithLabelValues("DynamoDB", "Scan", "Success").Observe(dynamoDuration)
	}

	// SQS ReceiveMessage
	sqsStart := time.Now()
	_, err = c.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: &c.sqsQueueURL,
	})
	sqsDuration := time.Since(sqsStart).Seconds()
	if ctx.Err() == context.Canceled {
		log.Printf("context is canceled")
		return
	} else if err != nil {
		log.Printf("failed to receive message, %v", err)
		requestDuration.WithLabelValues("SQS", "ReceiveMessage", "Failure").Observe(sqsDuration)
	} else {
		requestDuration.WithLabelValues("SQS", "ReceiveMessage", "Success").Observe(sqsDuration)
	}
}
