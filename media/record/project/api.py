def create_message(payload):
    message = payload["message"].strip()
    return 201, {"message": message}


def request(payload):
    try:
        return create_message(payload)
    except Exception:
        return 500, {"error": "internal server error"}
