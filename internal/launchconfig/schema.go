package launchconfig

const launchSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Clank web preview launch configuration",
  "type": "object",
  "additionalProperties": false,
  "required": ["default", "previews"],
  "properties": {
    "default": {
      "type": "string",
      "pattern": "^[A-Za-z0-9][A-Za-z0-9_-]*$"
    },
    "previews": {
      "type": "object",
      "minProperties": 1,
      "propertyNames": {
        "allOf": [
          {"pattern": "^[A-Za-z0-9][A-Za-z0-9_-]*$"},
          {"not": {"const": "default"}}
        ]
      },
      "additionalProperties": {"$ref": "#/$defs/preview"}
    }
  },
  "$defs": {
    "preview": {
      "type": "object",
      "additionalProperties": false,
      "required": ["directory", "command", "ready"],
      "properties": {
        "directory": {"type": "string", "minLength": 1},
        "command": {"type": "string", "minLength": 1},
        "env": {
          "type": "object",
          "propertyNames": {"pattern": "^[A-Za-z_][A-Za-z0-9_]*$"},
          "additionalProperties": {"type": "string"}
        },
        "ready": {"$ref": "#/$defs/ready"}
      }
    },
    "ready": {
      "type": "object",
      "additionalProperties": false,
      "required": ["path"],
      "properties": {
        "path": {"type": "string", "pattern": "^/[^?#]*$"},
        "expect": {"type": "string"}
      }
    }
  }
}`

// LaunchSchema returns the canonical structural schema shown to setup agents.
func LaunchSchema() string {
	return launchSchema
}
