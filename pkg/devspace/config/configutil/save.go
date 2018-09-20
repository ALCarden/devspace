package configutil

import (
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unsafe"

	"github.com/covexo/devspace/pkg/util/fsutil"
	"github.com/covexo/devspace/pkg/util/log"
	yaml "gopkg.in/yaml.v2"
)

//SaveConfig writes the data of a config to its yaml file
func SaveConfig() error {
	configExists, _ := ConfigExists()
	baseConfig := makeConfig()

	// just in case someone has set a pointer to one of the structs to nil, merge empty an empty config object into all configs
	merge(config, baseConfig, unsafe.Pointer(&config), unsafe.Pointer(baseConfig))
	merge(configRaw, baseConfig, unsafe.Pointer(&configRaw), unsafe.Pointer(baseConfig))
	merge(overwriteConfig, baseConfig, unsafe.Pointer(&overwriteConfig), unsafe.Pointer(baseConfig))
	merge(overwriteConfigRaw, baseConfig, unsafe.Pointer(&overwriteConfigRaw), unsafe.Pointer(baseConfig))

	configMapRaw, overwriteMapRaw, configErr := getConfigAndOverwriteMaps(config, configRaw, overwriteConfig, overwriteConfigRaw)

	configMap, _ := configMapRaw.(map[interface{}]interface{})
	overwriteMap, _ := overwriteMapRaw.(map[interface{}]interface{})

	if configErr != nil {
		return configErr
	}

	if config.Cluster.UseKubeConfig != nil && *config.Cluster.UseKubeConfig {
		clusterConfig := map[string]bool{
			"useKubeConfig": true,
		}
		_, configHasCluster := configMap["cluster"]

		if configHasCluster {
			configMap["cluster"] = clusterConfig
		}
		_, overwriteConfigHasCluster := overwriteMap["cluster"]

		if overwriteConfigHasCluster {
			overwriteMap["cluster"] = clusterConfig
		}
	}
	configYaml, yamlErr := yaml.Marshal(configMap)

	if yamlErr != nil {
		return yamlErr
	}
	configDir := filepath.Dir(workdir + configPath)

	os.MkdirAll(configDir, os.ModePerm)

	if !configExists {
		fsutil.WriteToFile([]byte(configGitignore), filepath.Join(configDir, ".gitignore"))
	}
	writeErr := ioutil.WriteFile(workdir+configPath, configYaml, os.ModePerm)

	if writeErr != nil {
		return writeErr
	}

	if overwriteMap != nil {
		overwriteConfigYaml, yamlErr := yaml.Marshal(overwriteMap)

		if yamlErr != nil {
			return yamlErr
		}
		return ioutil.WriteFile(workdir+overwriteConfigPath, overwriteConfigYaml, os.ModePerm)
	}
	return nil
}

func getConfigAndOverwriteMaps(config interface{}, configRaw interface{}, overwriteConfig interface{}, overwriteConfigRaw interface{}) (interface{}, interface{}, error) {

	object, isObjectNil := getPointerValue(config)

	objectType := reflect.TypeOf(object)
	objectKind := objectType.Kind()
	overwriteObject, isOverwriteObjectNil := getPointerValue(overwriteConfig)
	overwriteObjectKind := reflect.TypeOf(overwriteObject).Kind()

	if objectKind != overwriteObjectKind && !isObjectNil && !isOverwriteObjectNil {
		return nil, nil, errors.New("config (type: " + objectKind.String() + ") and overwriteConfig (type: " + overwriteObjectKind.String() + ") must be instances of the same type.")
	}
	objectValueRef := reflect.ValueOf(object)
	objectValue := objectValueRef.Interface()
	overwriteValueRef := reflect.ValueOf(overwriteObject)
	overwriteValue := overwriteValueRef.Interface()
	objectRaw, isObjectRawNil := getPointerValue(configRaw)
	objectRawValueRef := reflect.ValueOf(objectRaw)
	objectRawValue := objectRawValueRef.Interface()
	overwriteObjectRaw, _ := getPointerValue(overwriteConfigRaw)
	overwriteRawValueRef := reflect.ValueOf(overwriteObjectRaw)
	overwriteRawValue := overwriteRawValueRef.Interface()

	switch objectKind {
	case reflect.Slice:
		returnSlice := []interface{}{}
		returnOverwriteSlice := []interface{}{}
		var err error

	OUTER:
		for i := 0; i < objectValueRef.Len(); i++ {
			val := objectValueRef.Index(i).Interface()

			for ii := 0; ii < overwriteValueRef.Len(); ii++ {
				if val == overwriteValueRef.Index(ii).Interface() {
					continue OUTER
				}
			}

			if val != nil {
				//to remove nil values
				_, val, err = getConfigAndOverwriteMaps(val, val, val, val)

				if err != nil {
					return nil, nil, err
				}
				returnSlice = append(returnSlice, val)
			}
		}

		for i := 0; i < overwriteValueRef.Len(); i++ {
			val := overwriteValueRef.Index(i).Interface()

			if val != nil {
				//to remove nil values
				_, val, err = getConfigAndOverwriteMaps(val, val, val, val)

				if err != nil {
					return nil, nil, err
				}

				returnOverwriteSlice = append(returnOverwriteSlice, val)
			}
		}

		if len(returnSlice) > 0 && len(returnOverwriteSlice) > 0 {
			return returnSlice, returnOverwriteSlice, nil
		} else if len(returnSlice) > 0 {
			return returnSlice, nil, nil
		} else if len(returnOverwriteSlice) > 0 {
			return nil, returnOverwriteSlice, nil
		}
		return nil, nil, nil
	case reflect.Map:
		returnMap := map[interface{}]interface{}{}
		returnOverwriteMap := map[interface{}]interface{}{}
		genericPointerType := reflect.TypeOf(&returnMap)

		for _, keyRef := range objectValueRef.MapKeys() {
			key := keyRef.Interface()
			val := getMapValue(objectValue, key, genericPointerType)
			log.Info(key)
			yamlKey := getYamlKey(key.(string))
			valType := reflect.TypeOf(val)
			overwriteVal := getMapValue(overwriteValue, key, valType)

			if val != nil && overwriteVal != nil {
				valRaw := getMapValue(objectRawValue, key, valType)
				overwriteValRaw := getMapValue(overwriteRawValue, key, valType)
				var err error

				val, overwriteVal, err = getConfigAndOverwriteMaps(
					val,
					valRaw,
					overwriteVal,
					overwriteValRaw,
				)

				if err != nil {
					return nil, nil, err
				}
			}

			valRef := reflect.ValueOf(val)

			if !isZero(valRef) {
				returnMap[yamlKey] = val
			}

			overwriteValRef := reflect.ValueOf(overwriteVal)

			if !isZero(overwriteValRef) {
				returnOverwriteMap[yamlKey] = overwriteVal
			}
		}

		if len(returnMap) > 0 && len(returnOverwriteMap) > 0 {
			return returnMap, returnOverwriteMap, nil
		} else if len(returnMap) > 0 {
			return returnMap, nil, nil
		} else if len(returnOverwriteMap) > 0 {
			return nil, returnOverwriteMap, nil
		}
		return nil, nil, nil
	case reflect.Struct:
		returnMap := map[interface{}]interface{}{}
		returnOverwriteMap := map[interface{}]interface{}{}

		for i := 0; i < objectValueRef.NumField(); i++ {
			field := objectValueRef.Field(i)
			yamlKey := getYamlKey(objectValueRef.Type().Field(i).Name)

			if field.CanInterface() {
				fieldValue := field.Interface()
				fieldRawValue := objectRawValueRef.Field(i).Interface()
				overwriteFieldValue := overwriteValueRef.Field(i).Interface()
				overwriteRawFieldValue := overwriteRawValueRef.Field(i).Interface()

				fieldValueClean, overwriteFieldValueClean, err := getConfigAndOverwriteMaps(
					fieldValue,
					fieldRawValue,
					overwriteFieldValue,
					overwriteRawFieldValue,
				)

				if err != nil {
					return nil, nil, err
				}

				if fieldValueClean != nil {
					returnMap[yamlKey] = fieldValueClean
				}

				if overwriteFieldValueClean != nil {
					returnOverwriteMap[yamlKey] = overwriteFieldValueClean
				}
			}
		}

		if len(returnMap) > 0 && len(returnOverwriteMap) > 0 {
			return returnMap, returnOverwriteMap, nil
		} else if len(returnMap) > 0 {
			return returnMap, nil, nil
		} else if len(returnOverwriteMap) > 0 {
			return nil, returnOverwriteMap, nil
		}
		return nil, nil, nil
	default:
		saveOverwriteValue := !isOverwriteObjectNil
		saveValue := ((!isObjectNil && !saveOverwriteValue) || !isObjectRawNil)

		//TODO: Determine overwritten values and set objectValue accordingly

		if saveValue && saveOverwriteValue {
			return objectValue, overwriteValue, nil
		} else if saveOverwriteValue {
			return nil, overwriteValue, nil
		} else if saveValue {
			return objectValue, nil, nil
		}
		return nil, nil, nil
	}
}

func getMapValue(valueMap interface{}, key interface{}, refType reflect.Type) interface{} {
	valueMapValue, _ := getPointerValue(valueMap)
	mapRef := reflect.ValueOf(valueMapValue)
	keyRef := reflect.ValueOf(key)
	mapValue := mapRef.MapIndex(keyRef)

	if isZero(mapValue) {
		mapValue = reflect.New(refType)
	}
	return mapValue.Interface()
}

//isZero is a reflect function from: https://github.com/golang/go/issues/7501
func isZero(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Array, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Slice, reflect.Map, reflect.Interface, reflect.Ptr:
		return v.IsNil()
	}
	return false
}

func getYamlKey(key string) string {
	return strings.ToLower(key[0:1]) + key[1:]
}

func getPointerValue(object interface{}) (interface{}, bool) {
	if object != nil {
		objectType := reflect.TypeOf(object)
		objectKind := objectType.Kind()

		if objectKind == reflect.Ptr {
			objectValueRef := reflect.ValueOf(object)

			if objectValueRef.IsNil() {
				pointerValueType := objectValueRef.Type().Elem()
				newInstance := reflect.New(pointerValueType).Interface()

				return newInstance, true
			}
			return objectValueRef.Elem().Interface(), false
		}
	}
	return object, false
}
