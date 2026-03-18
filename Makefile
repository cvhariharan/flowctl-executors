PLUGINS_DIR := plugins
PLUGINS := http terraform

.PHONY: all clean $(PLUGINS)

all: $(PLUGINS)

$(PLUGINS):
	cd $@ && go build -o ../$(PLUGINS_DIR)/$@ .

clean:
	rm -rf $(PLUGINS_DIR)
