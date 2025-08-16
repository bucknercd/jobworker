CERTS_DIR := $(CURDIR)
CA_KEY := $(CERTS_DIR)/ca.key
CA_CERT := $(CERTS_DIR)/ca.crt
CA_SERIAL := $(CERTS_DIR)/ca.srl
SERVER_KEY := $(CERTS_DIR)/server.key
SERVER_CSR := $(CERTS_DIR)/server.csr
SERVER_CERT := $(CERTS_DIR)/server.crt

.PHONY: all server user clean

all: server

# Generate CA if not exists
$(CA_KEY) $(CA_CERT):
	@echo ">>> Generating CA"
	@openssl genrsa -out $(CA_KEY) 4096
	@openssl req -x509 -new -nodes -key $(CA_KEY) -sha256 -days 3650 \
		-subj "/CN=mtls-ca" \
		-out $(CA_CERT)

server: $(CA_KEY) $(CA_CERT)
	@if [ ! -f $(SERVER_KEY) ]; then \
		echo ">>> Generating server certs"; \
		openssl genrsa -out $(SERVER_KEY) 2048; \
		openssl req -new -key $(SERVER_KEY) \
			-subj "/CN=mtls-server" \
			-out $(SERVER_CSR); \
		openssl x509 -req -in $(SERVER_CSR) \
			-CA $(CA_CERT) -CAkey $(CA_KEY) -CAcreateserial \
			-out $(SERVER_CERT) -days 365 -sha256; \
		rm $(SERVER_CSR); \
	else echo ">>> Server certs already exist, skipping."; fi

user: $(CA_KEY) $(CA_CERT)
	@read -p "Enter username: " USERNAME; \
	USER_DIR=$(CERTS_DIR)/$$USERNAME; \
	if [ -f $$USER_DIR/client.crt ]; then \
		echo ">>> User $$USERNAME already has certs, skipping."; \
	else \
		echo ">>> Generating certs for user $$USERNAME"; \
		mkdir -p $$USER_DIR; \
		openssl genrsa -out $$USER_DIR/client.key 2048; \
		openssl req -new -key $$USER_DIR/client.key \
			-subj "/CN=$$USERNAME" \
			-out $$USER_DIR/client.csr; \
		openssl x509 -req -in $$USER_DIR/client.csr \
			-CA $(CA_CERT) -CAkey $(CA_KEY) -CAcreateserial \
			-out $$USER_DIR/client.crt -days 365 -sha256; \
		rm $$USER_DIR/client.csr; \
	fi

clean:
	@echo ">>> Cleaning all certs (but keeping the Makefile)"
	@find $(CERTS_DIR) -mindepth 1 ! -name 'Makefile' -exec rm -rf {} +