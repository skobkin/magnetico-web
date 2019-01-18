<?php

namespace App\Security;

use App\Api\V1\DTO\ApiResponse;
use App\Entity\User;
use App\Repository\ApiTokenRepository;
use App\Security\Token\AuthenticatedApiToken;
use Symfony\Component\HttpFoundation\{JsonResponse, Request, RequestStack, Response};
use Symfony\Component\Security\Core\Authentication\Token\TokenInterface;
use Symfony\Component\Security\Core\Exception\AuthenticationException;
use Symfony\Component\Security\Core\User\{UserInterface, UserProviderInterface};
use Symfony\Component\Security\Guard\AbstractGuardAuthenticator;
use Symfony\Component\Serializer\SerializerInterface;

class ApiTokenAuthenticator extends AbstractGuardAuthenticator
{
    public const TOKEN_HEADER = 'api-token';

    /** @var ApiTokenRepository */
    private $tokenRepo;

    /** @var SerializerInterface */
    private $serializer;

    /** @var RequestStack */
    private $requestStack;

    public function __construct(SerializerInterface $serializer, ApiTokenRepository $tokenRepo, RequestStack $requestStack)
    {
        $this->serializer = $serializer;
        $this->tokenRepo = $tokenRepo;
        // Crutch for Guard simplified auth to retrieve 'api-token' header in the createAuthenticatedToken()
        $this->requestStack = $requestStack;
    }

    public function supports(Request $request): bool
    {
        return $request->headers->has(self::TOKEN_HEADER);
    }

    public function getCredentials(Request $request)
    {
        return [
            'token' => $request->headers->get(self::TOKEN_HEADER),
        ];
    }

    public function getUser($credentials, UserProviderInterface $userProvider): ?User
    {
        if (null === $token = $credentials['token']) {
            return null;
        }

        return $this->tokenRepo->findUserByTokenKey($token);
    }

    public function start(Request $request, AuthenticationException $authException = null)
    {
        $message = sprintf('You need to use \'%s\' in your request: %s', self::TOKEN_HEADER, $authException ? $authException->getMessage() : '');

        $json = $this->serializer->serialize(
            new ApiResponse(null, JsonResponse::HTTP_UNAUTHORIZED, $message),
            'json',
            ['groups' => ['api']]
        );

        return new JsonResponse($json, Response::HTTP_UNAUTHORIZED,[], true);
    }

    public function checkCredentials($credentials, UserInterface $user)
    {
        // No credentials check needed in case of token auth
        return true;
    }

    public function onAuthenticationSuccess(Request $request, TokenInterface $token, $providerKey)
    {
        // No response object needed in token auth
        return null;
    }

    public function supportsRememberMe()
    {
        // Remember me functionality don't needed in token auth
        return false;
    }


    public function onAuthenticationFailure(Request $request, AuthenticationException $exception): JsonResponse
    {
        // @todo Decouple with App\Api\V1\DTO
        $json = $this->serializer->serialize(
            new ApiResponse(null, JsonResponse::HTTP_UNAUTHORIZED, $exception->getMessage()),
            'json',
            ['groups' => ['api']]
        );

        return new JsonResponse($json, JsonResponse::HTTP_UNAUTHORIZED,[], true);
    }

    public function createAuthenticatedToken(UserInterface $user, $providerKey)
    {
        $tokenKey = $this->requestStack->getCurrentRequest()->headers->get(self::TOKEN_HEADER);

        return new AuthenticatedApiToken(
            $user,
            $tokenKey,
            $providerKey,
            $user->getRoles()
        );
    }
}