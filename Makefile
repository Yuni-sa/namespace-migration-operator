BINARY_NAME := ns-operator

.PHONY: build
build:
	@echo "Building $(BINARY_NAME)..."
	@go build -o $(BINARY_NAME)

.PHONY: clean
clean:
	@echo "Cleaning..."
	@rm -f $(BINARY_NAME)

.PHONY: run
run:
	@echo "Running $(BINARY_NAME)..."
	@./$(BINARY_NAME)
