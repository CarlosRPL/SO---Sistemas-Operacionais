import requests
API_KEY = "AIzaSyCTgEmKht5NGFZ2-QVWgSTrleA1ZydrRBo"
import time

queries = [
    "sertao",
    "cangaco",
    "coronelismo",
    "quilombo",
    "engenho",
    "latifundio",
    "caatinga",
    "pantanal",
    "amazonia",
    "ribeirinho",
    "garimpo",
    "desmatamento",
    "sincretismo",
    "candomble",
    "umbanda",
    "terreiro",
    "folclore",
    "boitata",
    "curupira",
    "saci",
    "iara",
    "periferia",
    "favela",
    "migracao",
    "seca",
    "interior",
    "sertanejo",
    "modernismo",
    "regionalismo",
    "ditadura",
    "resistencia",
    "ancestralidade",
    "racismo",
    "desigualdade",
    "garoto",
    "orfao",
    "trabalho",
    "escravidao",
    "imigracao",
    "colonia",
    "capitania",
    "politica",
    "eleicoes",
    "corrupcao",
    "universidade",
    "estudante",
    "memoria",
    "familia",
    "identidade",
    "conflito",
    "machado",
    "clarice",
    "jorge amado",
    "graciliano",
    "guimaraes rosa",
    "drummond",
    "cecilia meireles",
    "manuel bandeira",
    "mario de andrade",
    "oswald",
    "lobato",
    "euclides",
    "lima barreto",
    "alencar",
    "castro alves",
    "azevedo",
    "raquel queiroz",
    "erico verissimo",
    "lygia telles",
    "hilda hilst",
    "joao cabral",
    "gullar",
    "rubem fonseca",
    "trevisan",
    "raduan",
    "hatoum",
    "chico buarque",
    "paulo coelho",
    "conceicao evaristo",
    "carolina jesus",
    "suassuna",
    "loyola brandao",
    "marcal aquino",
    "bernardo carvalho",
    "daniel galera",
    "raphael montes",
    "itamar vieira"
]

livros_unicos = set()

for query in queries:
    print(f"\nBuscando: {query}")
    
    for i in range(0, 1000, 40):  # limite da API
        url = f"https://www.googleapis.com/books/v1/volumes?q={query}&startIndex={i}&maxResults=40&key={API_KEY}"
        
        response = requests.get(url)
        
        if response.status_code != 200:
            print("Erro:", response.status_code)
            break
        
        data = response.json()
        
        items = data.get("items", [])
        if not items:
            break
        
        for item in items:
            info = item.get("volumeInfo", {})
            
            titulo = info.get("title", "Sem título")
            autores = ", ".join(info.get("authors", ["Desconhecido"]))
            
            livros_unicos.add(f"{titulo} — {autores}")
        
        print(f"  Pegou {i + 40}")
        time.sleep(0.2)  # evita bloqueio

with open("Books.txt", "w", encoding="utf-8") as f:
    livros_unicos=sorted(livros_unicos)
    for livro in livros_unicos:
        f.write(livro + "\n")

print(f"\nTotal de livros únicos: {len(livros_unicos)}")
print("Arquivo salvo!")