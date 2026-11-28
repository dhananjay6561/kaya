module github.com/dhananjay6561/kaya/core

go 1.26.0

require (
	github.com/dhananjay6561/kaya/contracts v0.0.0-00010101000000-000000000000
	github.com/dhananjay6561/kaya/kit v0.0.0
	github.com/jackc/pgx/v5 v5.13.0
	github.com/rivo/uniseg v0.4.7
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	golang.org/x/text v0.44.0
)

require (
	codeberg.org/readeck/go-readability/v2 v2.1.3 // indirect
	github.com/andybalholm/cascadia v1.3.3 // indirect
	github.com/go-shiori/dom v0.0.0-20230515143342-73569d674e1c // indirect
	github.com/gogs/chardet v0.0.0-20211120154057-b7413eaefb8f // indirect
	github.com/itlightning/dateparse v0.2.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/joho/godotenv v1.5.1 // indirect
	golang.org/x/net v0.63.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
)

replace (
	github.com/dhananjay6561/kaya/contracts => ../contracts
	github.com/dhananjay6561/kaya/kit => ../kit
)
