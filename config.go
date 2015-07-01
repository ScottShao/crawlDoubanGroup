package main

import (
	"encoding/json"
	"io/ioutil"
)

type Config map[string]interface{}

var configs = make(map[string]Config)

func ReadConf(path string) Config {
	if config, isExist := configs[path]; isExist {
		return config
	}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		panic(err)
	}

	config := make(Config)
	err = json.Unmarshal(data, &config)
	if err != nil {
		panic(err)
	}

	configs[path] = config
	return config
}

func (config Config) Get(key string) interface{} {
	if val, b := config[key]; b {
		return val
	}

	return nil
}

func (config Config) Set(key string, val interface{}) {
	config[key] = val
}
