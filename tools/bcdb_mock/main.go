package main

import (
	"context"
	"flag"

	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/threefoldtech/zos/pkg/app"
	"github.com/threefoldtech/zos/tools/bcdb_mock/mw"
	"github.com/threefoldtech/zos/tools/bcdb_mock/pkg/directory"
	"github.com/threefoldtech/zos/tools/bcdb_mock/pkg/escrow"
	escrowdb "github.com/threefoldtech/zos/tools/bcdb_mock/pkg/escrow/types"
	"github.com/threefoldtech/zos/tools/bcdb_mock/pkg/phonebook"
	"github.com/threefoldtech/zos/tools/bcdb_mock/pkg/stellar"
	"github.com/threefoldtech/zos/tools/bcdb_mock/pkg/workloads"
)

// Pkg is a shorthand type for func
type Pkg func(*mux.Router, *mongo.Database) error

func main() {
	app.Initialize()

	var (
		listen  string
		dbConf  string
		name    string
		seed    string
		network string
	)

	flag.StringVar(&listen, "listen", ":8080", "listen address, default :8080")
	flag.StringVar(&dbConf, "mongo", "mongodb://localhost:27017", "connection string to mongo database")
	flag.StringVar(&name, "name", "explorer", "database name")
	flag.StringVar(&seed, "seed", "", "wallet seed")
	flag.StringVar(&network, "network", "testnet", "tfchain network")
	flag.Parse()

	db, err := mw.NewDatabaseMiddleware(name, dbConf)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
	}

	router := mux.NewRouter()

	router.Use(db.Middleware)

	if err := escrowdb.Setup(context.Background(), db.Database()); err != nil {
		log.Fatal().Err(err).Msg("failed to create escrow database indexes")
	}

	wallet, err := stellar.New(seed, network)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create stellar wallet")
	}

	escrow := escrow.New(wallet, db.Database())
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create escrow")
	}

	go escrow.Run(context.Background())

	pkgs := []Pkg{
		phonebook.Setup,
		directory.Setup,
	}

	for _, pkg := range pkgs {
		if err := pkg(router, db.Database()); err != nil {
			log.Error().Err(err).Msg("failed to register package")
		}
	}

	if err = workloads.Setup(router, db.Database(), escrow); err != nil {
		log.Error().Err(err).Msg("failed to register package")
	}

	log.Printf("start on %s\n", listen)
	r := handlers.LoggingHandler(os.Stderr, router)
	r = handlers.CORS(handlers.AllowedOrigins([]string{"*"}))(r)

	s := &http.Server{
		Addr:    listen,
		Handler: r,
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go s.ListenAndServe()

	<-c

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	if err := s.Shutdown(ctx); err != nil {
		log.Printf("error during server shutdown: %v\n", err)
	}
}
