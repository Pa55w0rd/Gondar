package agent

func intentInputSchema(schema map[string]any) map[string]any {
	props := schema["properties"].(map[string]any)
	item := map[string]any{}
	for key, value := range props {
		if key != "intents" {
			item[key] = value
		}
	}
	props["intents"].(map[string]any)["items"] = obj(item, "summary")
	return schema
}
