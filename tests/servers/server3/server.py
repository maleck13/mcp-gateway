"""
A simple MCP server for testing, implemented using the FastMCP library.
See https://github.com/jlowin/fastmcp
"""

from datetime import datetime
import time as pytime # If we don't rename this, it confuses fastmcp

from fastmcp import FastMCP, Context
from fastmcp.server.dependencies import get_http_headers

mcp = FastMCP("FastMCP test server")

@mcp.tool
def time() -> str:
    """Return the current date and time."""
    return str(datetime.now())

@mcp.tool
def add(a: int, b: int) -> int:
    """Add two numbers"""
    return a + b

@mcp.tool
def dozen() -> int:
    """Return 12"""
    return 12

@mcp.tool
def pi() -> float:
    """Return 3.1415"""
    return 3.1415

@mcp.tool
def get_weather(city: str) -> dict:
    """Gets the current weather for a specific city."""
    # In a real app, this would call a weather API
    return {"city": city, "temperature": "72F", "forecast": "Sunny"}

@mcp.tool
async def slow(seconds: int, ctx: Context) -> str:
    """Wait for a specified number of seconds with progress notifications"""

    start_time = pytime.time()
    print(f"Slow tool will wait for {seconds} seconds")
    while True:
        waited = pytime.time() - start_time
        if waited >= seconds:
            break

        await ctx.report_progress(progress=int(waited), total=seconds)

        pytime.sleep(1)

    return ""

@mcp.tool
def get_headers() -> dict[str, str]:
    """Return the HTTP request headers received by the server."""
    # Note that get_http_headers returns init headers
    # See https://github.com/jlowin/fastmcp/issues/1233
    return get_http_headers(include_all=True)


# Fake restaurant data for testing
_restaurant_data = {
    "new york": [
        {"name": "Joe's Pizza", "cuisine": "Italian", "rating": 4.5, "address": "7 Carmine St", "available_time": "18:00"},
        {"name": "Le Bernardin", "cuisine": "French", "rating": 4.8, "address": "155 W 51st St", "available_time": "20:30"},
        {"name": "Xi'an Famous Foods", "cuisine": "Chinese", "rating": 4.3, "address": "81 St Marks Pl", "available_time": "19:00"},
    ],
    "london": [
        {"name": "Dishoom", "cuisine": "Indian", "rating": 4.6, "address": "12 Upper St Martin's Ln", "available_time": "19:30"},
        {"name": "The Wolseley", "cuisine": "European", "rating": 4.4, "address": "160 Piccadilly", "available_time": "21:00"},
        {"name": "Padella", "cuisine": "Italian", "rating": 4.7, "address": "6 Southwark St", "available_time": "18:30"},
    ],
    "tokyo": [
        {"name": "Sukiyabashi Jiro", "cuisine": "Sushi", "rating": 4.9, "address": "4-2-15 Ginza", "available_time": "17:00"},
        {"name": "Ichiran Shibuya", "cuisine": "Ramen", "rating": 4.5, "address": "1-22-7 Jinnan", "available_time": "12:00"},
        {"name": "Gonpachi", "cuisine": "Japanese", "rating": 4.3, "address": "1-13-11 Nishi-Azabu", "available_time": "19:00"},
    ],
    "paris": [
        {"name": "Le Comptoir du Panthéon", "cuisine": "French", "rating": 4.4, "address": "5 Rue Soufflot", "available_time": "20:00"},
        {"name": "L'As du Fallafel", "cuisine": "Middle Eastern", "rating": 4.6, "address": "34 Rue des Rosiers", "available_time": "13:00"},
        {"name": "Chez Janou", "cuisine": "French", "rating": 4.5, "address": "2 Rue Roger Verlomme", "available_time": "21:30"},
    ],
}


@mcp.tool
def restaurants(city: str) -> str:
    """List available restaurants in a city"""
    import json

    key = city.lower()
    listings = _restaurant_data.get(key)
    if listings is None:
        return f'No restaurant data available for "{city}". Try: New York, London, Tokyo, or Paris.'
    return json.dumps(listings, indent=2)


@mcp.tool
def book_restaurant(city: str, restaurant: str) -> str:
    """Book or reserve a table at a restaurant in a city"""
    import json

    key = city.lower()
    listings = _restaurant_data.get(key)
    if listings is None:
        return json.dumps({"error": f'Unknown city "{city}". Available cities: New York, London, Tokyo, Paris.'})

    for entry in listings:
        if entry["name"].lower() == restaurant.lower():
            ref = f"{key[:3]}-{hash(restaurant) % 10000:04d}"
            return json.dumps({"status": "confirmed", "restaurant": entry["name"], "city": city, "booking_reference": ref})

    return json.dumps({"error": f'Restaurant "{restaurant}" not found in {city}. Use the restaurants tool to list available options.'})


if __name__ == "__main__":
    # NOTE THIS NEVER GETS INVOKED.  WE RUN WITH THE FastMCP harness:
    # fastmcp run server.py --transport http
    print("Syntax: Use `fastmcp run server.py --transport http` instead.")
