package main

import (
	"log"

	"notification-job-mb/internal/config"
	"notification-job-mb/internal/database"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := database.Connect(cfg.DB)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := database.Close(db); cerr != nil {
			log.Printf("close database: %v", cerr)
		}
	}()

	log.Println("running database migration...")
	if err := database.Migrate(db); err != nil {
		return err
	}

	log.Println("database migration completed")
	return nil
}
